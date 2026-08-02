import assert from "node:assert/strict";
import test from "node:test";

import {
  buildRealmEmailRouteProjection,
  realmEmailRouteKey as controlPlaneRouteKey,
} from "../../control-plane/src/realm-email-alias-runtime.mjs";
import {
  realmRouteKey as edgeRouteKey,
  validateRealmRouteProjection,
} from "../src/directory.mjs";

const domain = "witmail.ai";
const realmID = "realm_abcdefghijkl2345";
const updatedAt = "2026-08-01T12:00:00.000Z";

function controlPlaneProjection(realmLabel, routeKind, state = "applied") {
  return buildRealmEmailRouteProjection({
    domain,
    realm_id: realmID,
    realm_label: realmLabel,
    route_kind: routeKind,
    state,
    ...(state === "suspended"
      ? { suspension_disposition: "retry" }
      : {}),
    controller_revision: 23,
    updated_at: updatedAt,
    cache_ttl_seconds: 300,
    cell_audience: "gcp-prod-us-central1-core",
    ingest_url: "https://api.cell.example/v1/internal/agent-email:ingest",
  });
}

test("control-plane publisher and email edge share one canonical route contract", () => {
  for (const [realmLabel, routeKind] of [
    ["abcdefghijkl2345", "canonical"],
    ["acme-west", "realm_alias"],
  ]) {
    assert.equal(controlPlaneRouteKey(domain, realmLabel), edgeRouteKey(domain, realmLabel));
    const published = controlPlaneProjection(realmLabel, routeKind);
    assert.deepEqual(
      validateRealmRouteProjection(published, domain, realmLabel),
      published,
    );
  }
});

test("control-plane suspension and retirement omit cell destinations accepted by edge", () => {
  for (const state of ["suspended", "retired"]) {
    const published = controlPlaneProjection("acme-west", "realm_alias", state);
    assert.equal("cell_audience" in published, false);
    assert.equal("ingest_url" in published, false);
    assert.deepEqual(
      validateRealmRouteProjection(published, domain, "acme-west"),
      published,
    );
  }
});
