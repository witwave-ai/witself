import assert from "node:assert/strict";
import test from "node:test";

import {
  agentEmailRouteSignatureInput as controlPlaneSignatureInput,
  signAgentEmailRouteProjection,
} from "../../control-plane/src/agent-email-route-signature.mjs";
import {
  verifyAgentEmailRouteProjectionSignature,
} from "../src/route-signature.mjs";
import {
  ROUTE_PUBLIC_KEY_ENV,
  ROUTE_SIGNING_KEY_ID,
  ROUTE_SIGNING_PKCS8_BASE64,
  signTestRouteProjection,
} from "./route-signature-fixture.mjs";

const projection = {
  schema_version: 1,
  domain: "witmail.net",
  realm_label: "acme-west",
  realm_id: "realm_abcdefghijkl2345",
  route_kind: "realm_alias",
  state: "applied",
  controller_revision: 23,
  updated_at: "2026-08-09T12:00:00.000Z",
  cache_ttl_seconds: 300,
  cell_audience: "gcp-prod-us-central1-core",
  ingest_url: "https://api.cell.example/v1/internal/agent-email:ingest",
};

const signingEnv = {
  AGENT_EMAIL_ROUTE_SIGNING_KEY_ID: ROUTE_SIGNING_KEY_ID,
  AGENT_EMAIL_ROUTE_ED25519_PRIVATE_KEY: ROUTE_SIGNING_PKCS8_BASE64,
};
const verificationEnv = {
  AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: ROUTE_PUBLIC_KEY_ENV,
};

test("control plane and edge share one deterministic signed-route vector", async () => {
  const signed = await signAgentEmailRouteProjection(projection, signingEnv);
  assert.deepEqual(signed, signTestRouteProjection(projection));
  assert.deepEqual(
    await verifyAgentEmailRouteProjectionSignature(signed, verificationEnv),
    projection,
  );
  assert.equal(
    Buffer.from(controlPlaneSignatureInput(projection, ROUTE_SIGNING_KEY_ID))
      .toString("base64"),
    "d2l0c2VsZi1hZ2VudC1lbWFpbC1yb3V0ZS1wcm9qZWN0aW9uLXYxCnsiY2FjaGVfdHRsX3NlY29uZHMiOjMwMCwiY2VsbF9hdWRpZW5jZSI6ImdjcC1wcm9kLXVzLWNlbnRyYWwxLWNvcmUiLCJjb250cm9sbGVyX3JldmlzaW9uIjoyMywiZG9tYWluIjoid2l0bWFpbC5uZXQiLCJpbmdlc3RfdXJsIjoiaHR0cHM6Ly9hcGkuY2VsbC5leGFtcGxlL3YxL2ludGVybmFsL2FnZW50LWVtYWlsOmluZ2VzdCIsInJlYWxtX2lkIjoicmVhbG1fYWJjZGVmZ2hpamtsMjM0NSIsInJlYWxtX2xhYmVsIjoiYWNtZS13ZXN0Iiwicm91dGVfa2luZCI6InJlYWxtX2FsaWFzIiwicm91dGVfc2lnbmluZ19rZXlfaWQiOiJyb3V0ZS0yMDI2LTA4Iiwic2NoZW1hX3ZlcnNpb24iOjIsInN0YXRlIjoiYXBwbGllZCIsInVwZGF0ZWRfYXQiOiIyMDI2LTA4LTA5VDEyOjAwOjAwLjAwMFoifQo=",
  );
});

test("route signature rejects mutation, unknown keys, and unsigned v1 rows", async () => {
  const signed = signTestRouteProjection(projection);
  await assert.rejects(
    verifyAgentEmailRouteProjectionSignature(
      { ...signed, ingest_url: "https://attacker.example/ingest" },
      verificationEnv,
    ),
    /verification failed/,
  );
  await assert.rejects(
    verifyAgentEmailRouteProjectionSignature(
      { ...signed, route_signing_key_id: "unknown-key" },
      verificationEnv,
    ),
    /not trusted/,
  );
  await assert.rejects(
    verifyAgentEmailRouteProjectionSignature(projection, verificationEnv),
    /signed agent email route projection is invalid/,
  );
});

test("route key configuration is strict and bounded for rotation", async () => {
  const signed = signTestRouteProjection(projection);
  for (const value of [
    "",
    "{}",
    "not-json",
    ` {"${ROUTE_SIGNING_KEY_ID}":"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`,
    `{"${ROUTE_SIGNING_KEY_ID}":"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=","${ROUTE_SIGNING_KEY_ID}":"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`,
    `{"route-z":"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=","route-a":"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="}`,
    JSON.stringify({ bad: "not-base64" }),
    JSON.stringify(Object.fromEntries(
      Array.from({ length: 5 }, (_, index) => [
        `route-${index}`,
        "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=",
      ]),
    )),
  ]) {
    await assert.rejects(
      verifyAgentEmailRouteProjectionSignature(signed, {
        AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: value,
      }),
    );
  }
});
