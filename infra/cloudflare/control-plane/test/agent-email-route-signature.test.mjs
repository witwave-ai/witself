import assert from "node:assert/strict";
import test from "node:test";

import {
  AgentEmailRouteSigningError,
  agentEmailRouteSignatureInput,
  signAgentEmailRouteProjection,
} from "../src/agent-email-route-signature.mjs";

const projection = {
  schema_version: 1,
  domain: "agents.example.com",
  realm_label: "acme-west",
  realm_id: "realm_abcdefghijkl2345",
  route_kind: "custom_domain",
  state: "applied",
  controller_revision: 23,
  updated_at: "2026-08-09T12:00:00.000Z",
  cache_ttl_seconds: 300,
  domain_request_id: "aedr_aaaaaaaaaaaaaaaa",
  domain_allocation_revision: 11,
  realm_alias_claim_id: "era_bbbbbbbbbbbbbbbb",
  realm_alias_revision: 19,
  cell_audience: "gcp-prod-us-central1-core",
  ingest_url: "https://api.cell.example/v1/internal/agent-email:ingest",
};
const env = {
  AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: "route-2026-08",
  AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY:
    "MC4CAQAwBQYDK2VwBCIEIJ1hsZ3v/VpguoRK9JLsLMREScVpezJpGXA7rAMcrn9g",
};

test("route signer emits deterministic schema-v2 authority", async () => {
  const first = await signAgentEmailRouteProjection(projection, env);
  const second = await signAgentEmailRouteProjection(projection, env);
  assert.deepEqual(first, second);
  assert.equal(first.schema_version, 2);
  assert.equal(first.route_signing_key_id, "route-2026-08");
  assert.match(first.route_signature, /^[A-Za-z0-9+/]{86}==$/);
  assert.equal(first.ingest_url, projection.ingest_url);
  assert.equal(
    new TextDecoder().decode(
      agentEmailRouteSignatureInput(projection, "route-2026-08"),
    ).startsWith("witself-agent-email-route-projection-v1\n{"),
    true,
  );
});

test("route signer fails closed on missing keys and pre-signed input", async () => {
  await assert.rejects(
    signAgentEmailRouteProjection(projection, {}),
    AgentEmailRouteSigningError,
  );
  await assert.rejects(
    signAgentEmailRouteProjection(projection, {
      ...env,
      AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: " ROUTE ",
    }),
    AgentEmailRouteSigningError,
  );
  await assert.rejects(
    signAgentEmailRouteProjection(
      { ...projection, route_signature: `${"A".repeat(86)}==` },
      env,
    ),
    AgentEmailRouteSigningError,
  );
});
