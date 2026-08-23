import assert from "node:assert/strict";
import test from "node:test";

import {
  EDGE_METRICS_SCHEMA,
  recordEdgeVerdict,
  recordRouteLookup,
  ROUTE_LOOKUP_METRICS_SCHEMA,
} from "../src/metrics.mjs";

const release = {
  WITSELF_EDGE_RELEASE_VERSION: "1.2.3",
  WITSELF_EDGE_RELEASE_COMMIT: "a".repeat(40),
  WITSELF_EDGE_RELEASE_DATE: "2026-08-09T12:00:00Z",
};

function metricsEnv(extra = {}) {
  const points = [];
  return {
    points,
    env: {
      EMAIL_EDGE_METRICS: {
        writeDataPoint(point) {
          points.push(point);
        },
      },
      ...extra,
    },
  };
}

test("edge metrics append complete release attribution without changing indexes", () => {
  const { env, points } = metricsEnv(release);
  recordEdgeVerdict(env, {
    outcome: "accepted",
    phase: "response",
    durationMS: 4,
  });
  recordRouteLookup(env, {
    result: "cp_error",
    evidence: "none",
    routeKind: "custom_domain",
    durationMS: 3,
    status: 503,
  });

  assert.deepEqual(points[0].indexes, ["accepted"]);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA,
    "accepted",
    "response",
    "1.2.3",
    "a".repeat(40),
    "2026-08-09T12:00:00Z",
  ]);
  assert.deepEqual(points[1].indexes, ["cp_error"]);
  assert.deepEqual(points[1].blobs, [
    ROUTE_LOOKUP_METRICS_SCHEMA,
    "cp_error",
    "none",
    "custom_domain",
    "1.2.3",
    "a".repeat(40),
    "2026-08-09T12:00:00Z",
  ]);
});

test("edge metrics preserve the DMARC rejection outcome and authentication phase", () => {
  const { env, points } = metricsEnv();
  recordEdgeVerdict(env, {
    outcome: "rejected_dmarc_fail",
    phase: "authentication",
    durationMS: 2,
    rawSize: 512,
    status: 550,
  });

  assert.deepEqual(points[0].indexes, ["rejected_dmarc_fail"]);
  assert.deepEqual(points[0].blobs, [
    EDGE_METRICS_SCHEMA,
    "rejected_dmarc_fail",
    "authentication",
  ]);
  assert.deepEqual(points[0].doubles, [1, 2, 512, 550]);
});

test("edge metrics omit incomplete or malformed release metadata", () => {
  for (const invalid of [
    {},
    { ...release, WITSELF_EDGE_RELEASE_VERSION: "tenant supplied" },
    { ...release, WITSELF_EDGE_RELEASE_COMMIT: "not-a-commit" },
    { ...release, WITSELF_EDGE_RELEASE_DATE: "not-a-date" },
  ]) {
    const { env, points } = metricsEnv(invalid);
    recordRouteLookup(env, {
      result: "cp_error",
      evidence: "none",
      routeKind: "custom_domain",
    });
    assert.deepEqual(points[0].blobs, [
      ROUTE_LOOKUP_METRICS_SCHEMA,
      "cp_error",
      "none",
      "custom_domain",
    ]);
  }
});
