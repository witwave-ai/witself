import assert from "node:assert/strict";
import test from "node:test";

import {
  buildRealmEmailRouteProjection,
  realmEmailRouteKey as controlPlaneRouteKey,
} from "../../control-plane/src/realm-email-alias-runtime.mjs";
import {
  agentEmailCustomDomainRouteKey,
  buildAgentEmailCustomDomainRouteProjection,
} from "../../control-plane/src/agent-email-custom-domain-route-contract.mjs";
import {
  realmRouteKey as edgeRouteKey,
  validateRealmRouteProjection,
} from "../src/directory.mjs";

const domain = "witmail.net";
const realmID = "realm_abcdefghijkl2345";
const updatedAt = "2026-08-01T12:00:00.000Z";
const accountID = "acct_email_canary";

function controlPlaneProjection(realmLabel, routeKind, state = "applied") {
  return buildRealmEmailRouteProjection({
    account_id: accountID,
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

test("control-plane and edge share the exact custom-domain union variant", () => {
  const customDomain = "agents.example.com";
  const published = buildAgentEmailCustomDomainRouteProjection({
    domain: customDomain,
    realm_id: realmID,
    realm_label: "acme-west",
    domain_request_id: "aedr_aaaaaaaaaaaaaaaa",
    domain_allocation_revision: 11,
    realm_alias_claim_id: "era_bbbbbbbbbbbbbbbb",
    realm_alias_revision: 19,
    state: "applied",
    controller_revision: 23,
    updated_at: updatedAt,
    cache_ttl_seconds: 300,
    cell_audience: "gcp-prod-us-central1-core",
    ingest_url: "https://api.cell.example/v1/internal/agent-email:ingest",
  });
  assert.equal(
    agentEmailCustomDomainRouteKey(customDomain, "acme-west"),
    edgeRouteKey(customDomain, "acme-west"),
  );
  assert.deepEqual(
    validateRealmRouteProjection(published, customDomain, "acme-west"),
    published,
  );
});
