import assert from "node:assert/strict";
import test from "node:test";

import {
  parseRouteAddress,
  realmRouteKey,
  realmRouteProjectionIsFresh,
  validateRealmRouteProjection,
} from "../src/directory.mjs";

const domain = "agent-mail.witwave.ai";
const realmID = "realm_abcdefghijkl2345";
const canonicalLabel = "abcdefghijkl2345";
const aliasLabel = "acme-west";
const nowMS = Date.parse("2026-08-01T12:00:00.000Z");

function projection(realmLabel, overrides = {}) {
  return {
    schema_version: 1,
    domain,
    realm_label: realmLabel,
    realm_id: realmID,
    route_kind: realmLabel === canonicalLabel ? "canonical" : "realm_alias",
    state: "applied",
    controller_revision: 17,
    updated_at: new Date(nowMS).toISOString(),
    cache_ttl_seconds: 300,
    cell_audience: "gcp-prod-us-central1-core",
    ingest_url: "https://api.cell.example/v1/internal/agent-email:ingest",
    ...overrides,
  };
}

test("canonical and realm-alias addresses share one strict route shape", () => {
  const canonical = parseRouteAddress(`alpha.${canonicalLabel}@${domain}`, true);
  const alias = parseRouteAddress(`alpha.${aliasLabel}+signup@${domain}`, true);
  assert.equal(canonical.realmLabel, canonicalLabel);
  assert.equal(alias.realmLabel, aliasLabel);
  assert.equal(alias.agentSegment, canonical.agentSegment);
  assert.equal(alias.tag, "signup");

  const canonicalRoute = validateRealmRouteProjection(
    projection(canonicalLabel),
    domain,
    canonicalLabel,
  );
  const aliasRoute = validateRealmRouteProjection(projection(aliasLabel), domain, aliasLabel);
  assert.equal(canonicalRoute.realm_id, aliasRoute.realm_id);
  assert.equal(canonicalRoute.cell_audience, aliasRoute.cell_audience);
  assert.equal(canonicalRoute.ingest_url, aliasRoute.ingest_url);
  assert.equal(canonicalRoute.route_kind, "canonical");
  assert.equal(aliasRoute.route_kind, "realm_alias");
});

test("suspended and retired routes carry no destination authority", () => {
  for (const state of ["suspended", "retired"]) {
    const value = projection(aliasLabel, {
      state,
      ...(state === "suspended"
        ? { suspension_disposition: "retry" }
        : {}),
    });
    delete value.cell_audience;
    delete value.ingest_url;
    const route = validateRealmRouteProjection(value, domain, aliasLabel);
    assert.equal(route.state, state);
    assert.equal("cell_audience" in route, false);
    assert.equal("ingest_url" in route, false);
  }

  assert.throws(
    () => validateRealmRouteProjection(projection(aliasLabel, { state: "suspended" }), domain, aliasLabel),
    /schema is invalid/,
  );
});

test("projection rejects malformed, misbound, or identity-bearing rows", () => {
  for (const [changed, pattern] of [
    [{ route_kind: "canonical" }, /kind is inconsistent/],
    [{ realm_id: "realm_0000000000000000" }, /realm id is invalid/],
    [{ state: "active" }, /schema is invalid|state is invalid/],
    [{ account_id: "account_aaaaaaaaaaaaaaaa" }, /schema is invalid/],
    [{ agent_id: "agent_aaaaaaaaaaaaaaa2" }, /schema is invalid/],
    [{ domain: "other.example" }, /lookup binding is inconsistent/],
    [{ realm_label: "other-realm" }, /lookup binding is inconsistent/],
    [{ ingest_url: "https://api.cell.example/ingest?realm=other" }, /ingestion URL/],
  ]) {
    assert.throws(
      () => validateRealmRouteProjection(projection(aliasLabel, changed), domain, aliasLabel),
      pattern,
    );
  }

  for (const address of [
    `alpha.bad--alias@${domain}`,
    `alpha.xn--acme@${domain}`,
    `alpha.one.two@${domain}`,
    `Alpha.${aliasLabel}@${domain}`,
    `admin.${aliasLabel}@${domain}`,
    `${"a".repeat(40)}.${aliasLabel}+${"t".repeat(20)}@${domain}`,
  ]) {
    assert.throws(() => parseRouteAddress(address, true));
  }
});

test("route keys and repeated lookup bindings are collision safe", () => {
  const canonicalKey = realmRouteKey(domain, canonicalLabel);
  const aliasKey = realmRouteKey(domain, aliasLabel);
  assert.equal(canonicalKey, `email:realm-route:v1:${domain}:${canonicalLabel}`);
  assert.notEqual(canonicalKey, aliasKey);
  assert.notEqual(aliasKey, realmRouteKey("other.example", aliasLabel));

  assert.throws(
    () => validateRealmRouteProjection(projection(aliasLabel), domain, canonicalLabel),
    /lookup binding is inconsistent/,
  );
  assert.throws(() => realmRouteKey(domain, "acme:west"), /label is invalid/);
});

test("route freshness is bounded by timestamp and ttl", () => {
  const route = validateRealmRouteProjection(projection(aliasLabel), domain, aliasLabel);
  assert.equal(realmRouteProjectionIsFresh(route, nowMS), true);
  assert.equal(realmRouteProjectionIsFresh(route, nowMS + 300_000), true);
  assert.equal(realmRouteProjectionIsFresh(route, nowMS + 300_001), false);
  assert.equal(realmRouteProjectionIsFresh({ ...route, updated_at: new Date(nowMS + 300_001).toISOString() }, nowMS), false);
});
