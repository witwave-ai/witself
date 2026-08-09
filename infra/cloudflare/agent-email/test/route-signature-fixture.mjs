import { createPrivateKey, sign } from "node:crypto";

import {
  agentEmailRouteSignatureInput,
  agentEmailRouteSignaturePayload,
} from "../src/route-signature.mjs";

export const ROUTE_SIGNING_KEY_ID = "route-2026-08";
export const ROUTE_SIGNING_PKCS8_BASE64 =
  "MC4CAQAwBQYDK2VwBCIEIJ1hsZ3v/VpguoRK9JLsLMREScVpezJpGXA7rAMcrn9g";
export const ROUTE_SIGNING_PUBLIC_KEY_BASE64 =
  "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=";
export const ROUTE_PUBLIC_KEY_ENV = JSON.stringify({
  [ROUTE_SIGNING_KEY_ID]: ROUTE_SIGNING_PUBLIC_KEY_BASE64,
});

const privateKey = createPrivateKey({
  key: Buffer.from(ROUTE_SIGNING_PKCS8_BASE64, "base64"),
  format: "der",
  type: "pkcs8",
});

export function signTestRouteProjection(projection) {
  const payload = agentEmailRouteSignaturePayload(
    projection,
    ROUTE_SIGNING_KEY_ID,
  );
  const signature = sign(
    null,
    agentEmailRouteSignatureInput(projection, ROUTE_SIGNING_KEY_ID),
    privateKey,
  );
  return {
    ...payload,
    route_signature: signature.toString("base64"),
  };
}

export function routeVerificationEnv(overrides = {}) {
  return {
    AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS: ROUTE_PUBLIC_KEY_ENV,
    ...overrides,
  };
}
