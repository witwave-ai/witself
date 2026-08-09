import assert from "node:assert/strict";
import test from "node:test";

import {
  agentEmailCustomDomainRouteKey,
  buildAgentEmailCustomDomainCellProjection,
  buildAgentEmailCustomDomainRouteProjection,
  buildRealmEmailAliasClaimProof,
  cellAgentEmailCustomDomainRouteURL,
  validateAgentEmailCustomDomainCellProjection,
  validateRealmEmailAliasClaimProof,
} from "../src/agent-email-custom-domain-route-contract.mjs";

const ACCOUNT = "acct_custom_route";
const REALM = "realm_aaaaaaaaaaaaaaaa";
const REQUEST = "aedr_aaaaaaaaaaaaaaaa";
const CLAIM = "era_aaaaaaaaaaaaaaaa";
const DOMAIN = "agents.customer.example";
const ALIAS = "customer-team";

function cellProjection(fields = {}) {
  return buildAgentEmailCustomDomainCellProjection({
    account_id: ACCOUNT,
    domain: DOMAIN,
    realm_label: ALIAS,
    realm_id: REALM,
    domain_request_id: REQUEST,
    domain_allocation_revision: 4,
    domain_state_revision: 7,
    realm_alias_claim_id: CLAIM,
    realm_alias_revision: 9,
    controller_revision: 20,
    state: "applied",
    ...fields,
  });
}

test("custom-domain cell contract binds every independent authority revision", () => {
  const projection = cellProjection();
  assert.deepEqual(Object.keys(projection).sort(), [
    "account_id", "controller_revision", "domain",
    "domain_allocation_revision", "domain_request_id",
    "domain_state_revision", "realm_alias_claim_id", "realm_alias_revision",
    "realm_id", "realm_label", "schema_version", "state",
  ].sort());
  assert.equal(projection.schema_version, "witself.v0");
  assert.deepEqual(
    validateAgentEmailCustomDomainCellProjection(projection, projection),
    projection,
  );
  for (const mutation of [
    { domain_allocation_revision: 3 },
    { domain_state_revision: 6 },
    { realm_alias_claim_id: "era_bbbbbbbbbbbbbbbb" },
    { realm_alias_revision: 8 },
    { realm_id: "realm_bbbbbbbbbbbbbbbb" },
    { state: "suspended", suspension_disposition: "retry" },
  ]) {
    assert.throws(
      () => validateAgentEmailCustomDomainCellProjection(
        { ...projection, ...mutation },
        projection,
      ),
      /inconsistent/,
    );
  }
  assert.throws(
    () => validateAgentEmailCustomDomainCellProjection({
      ...projection,
      unexpected: true,
    }),
    /invalid/,
  );
});

test("custom-domain edge projection is the strict existing route union", () => {
  const projection = buildAgentEmailCustomDomainRouteProjection({
    ...cellProjection(),
    updated_at: "2026-08-09T12:00:00.000Z",
    cell_audience: "cell-one",
    ingest_url: "https://cell.example/v1/internal/agent-email:ingest",
  });
  assert.deepEqual(Object.keys(projection).sort(), [
    "cache_ttl_seconds", "cell_audience", "controller_revision", "domain",
    "domain_allocation_revision", "domain_request_id", "ingest_url",
    "realm_alias_claim_id", "realm_alias_revision", "realm_id",
    "realm_label", "route_kind", "schema_version", "state", "updated_at",
  ].sort());
  assert.equal(projection.route_kind, "custom_domain");
  assert.equal(
    agentEmailCustomDomainRouteKey(DOMAIN, ALIAS),
    `email:realm-route:v1:${DOMAIN}:${ALIAS}`,
  );
  for (const state of ["suspended", "retired"]) {
    const nonApplied = buildAgentEmailCustomDomainRouteProjection({
      ...cellProjection({
        state,
        ...(state === "suspended"
          ? { suspension_disposition: "inactive" }
          : {}),
      }),
      updated_at: "2026-08-09T12:00:00.000Z",
    });
    assert.equal("cell_audience" in nonApplied, false);
    assert.equal("ingest_url" in nonApplied, false);
  }
});

test("realm-alias proof is exact, read-only shaped, and caller-bound", () => {
  const proof = buildRealmEmailAliasClaimProof({
    account_id: ACCOUNT,
    realm_id: REALM,
    realm_label: ALIAS,
    realm_alias_claim_id: CLAIM,
    realm_alias_revision: 9,
    state: "suspended",
    suspension_disposition: "retry",
    updated_at: "2026-08-09T12:00:00.000Z",
  });
  assert.deepEqual(
    validateRealmEmailAliasClaimProof(proof, ACCOUNT, ALIAS),
    proof,
  );
  assert.throws(
    () => validateRealmEmailAliasClaimProof(proof, "acct_other", ALIAS),
    /inconsistent/,
  );
  assert.throws(
    () => validateRealmEmailAliasClaimProof({ ...proof, extra: true }, ACCOUNT, ALIAS),
    /invalid/,
  );
});

test("custom-domain routes cannot occupy the canonical realm-label namespace", () => {
  const canonicalLabel = "aaaaaaaaaaaaaaaa";
  assert.throws(
    () => agentEmailCustomDomainRouteKey(DOMAIN, canonicalLabel),
    /realm label is invalid/,
  );
  assert.throws(
    () => cellProjection({ realm_label: canonicalLabel }),
    /realm label is invalid/,
  );
  assert.throws(
    () => buildRealmEmailAliasClaimProof({
      account_id: ACCOUNT,
      realm_id: REALM,
      realm_label: canonicalLabel,
      realm_alias_claim_id: CLAIM,
      realm_alias_revision: 9,
      state: "applied",
      updated_at: "2026-08-09T12:00:00.000Z",
    }),
    /realm label is invalid/,
  );
});

test("cell apply and exact readback share one endpoint", () => {
  assert.equal(
    cellAgentEmailCustomDomainRouteURL("https://cell.example", ACCOUNT),
    `https://cell.example/v1/accounts/${ACCOUNT}:email-custom-domain-route`,
  );
  assert.equal(
    cellAgentEmailCustomDomainRouteURL(
      "https://cell.example",
      ACCOUNT,
      REQUEST,
      CLAIM,
    ),
    `https://cell.example/v1/accounts/${ACCOUNT}:email-custom-domain-route?` +
      `domain_request_id=${REQUEST}&realm_alias_claim_id=${CLAIM}`,
  );
});
