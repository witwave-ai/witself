#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, join, resolve } from "node:path";

import { sourceIdentity } from "./source-identity.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../src/managed-delivery-cohort.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const namespaceID = String(process.env.EMAIL_DIRECTORY_KV_ID ?? "");
const keyID = String(process.env.RELAY_KEY_ID ?? "").trim().toLowerCase();
const rawControlPlaneURL = String(process.env.CONTROL_PLANE_URL ?? "");
const aliasDeliveryEnabled = String(
  process.env.REALM_EMAIL_ALIAS_DELIVERY_ENABLED ?? "",
);
const canonicalDeliveryEnabled = String(
  process.env.REALM_EMAIL_CANONICAL_DELIVERY_ENABLED ?? "",
);
const dmarcRejectEnabled = String(
  process.env.AGENT_EMAIL_DMARC_REJECT_ENABLED ?? "false",
);
const authenticationResultsAuthservID = String(
  process.env.AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID ?? "",
);
const relayVersion = String(
  process.env.AGENT_EMAIL_RELAY_VERSION ?? "witself-email-relay-pilot-v1",
);
const rawRoutePublicKeys = String(
  process.env.AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS ?? "",
);
const managedDeliveryAccountAllowlist = String(
  process.env.AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST ?? "",
);
parseManagedDeliveryAccountAllowlist(managedDeliveryAccountAllowlist);
const release = sourceIdentity();

function outputPath(argv) {
  if (argv.length === 0) return join(root, "wrangler.generated.jsonc");
  if (argv.length !== 2 || argv[0] !== "--output" || !argv[1]) {
    throw new Error("usage: render-wrangler.mjs [--output PATH]");
  }
  return resolve(root, argv[1]);
}
const renderedOutput = outputPath(process.argv.slice(2));
if (!/^[0-9a-f]{32}$/.test(namespaceID)) throw new Error("EMAIL_DIRECTORY_KV_ID must be a 32-character lowercase hex id");
if (!/^[a-z][a-z0-9_-]{0,63}$/.test(keyID)) throw new Error("RELAY_KEY_ID is missing or invalid");
if (!["true", "false"].includes(aliasDeliveryEnabled)) {
  throw new Error("REALM_EMAIL_ALIAS_DELIVERY_ENABLED must be true or false");
}
if (!["true", "false"].includes(canonicalDeliveryEnabled)) {
  throw new Error("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED must be true or false");
}
if (!["true", "false"].includes(dmarcRejectEnabled)) {
  throw new Error("AGENT_EMAIL_DMARC_REJECT_ENABLED must be true or false");
}
if (
  authenticationResultsAuthservID !== "" &&
  !/^[\x21-\x3a\x3c-\x7e]{1,255}$/.test(authenticationResultsAuthservID)
) {
  throw new Error(
    "AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID must be empty or a printable non-space ASCII token without semicolons",
  );
}
if (!["witself-email-relay-pilot-v1", "witself-email-relay-v2"].includes(relayVersion)) {
  throw new Error(
    "AGENT_EMAIL_RELAY_VERSION must be witself-email-relay-pilot-v1 or witself-email-relay-v2",
  );
}
let controlPlaneURL;
try {
  controlPlaneURL = new URL(rawControlPlaneURL);
} catch {
  throw new Error("CONTROL_PLANE_URL must be a credential-free public HTTPS origin");
}
if (
  rawControlPlaneURL !== rawControlPlaneURL.trim() ||
  controlPlaneURL.protocol !== "https:" ||
  controlPlaneURL.username || controlPlaneURL.password ||
  controlPlaneURL.search || controlPlaneURL.hash ||
  !controlPlaneURL.hostname || controlPlaneURL.hostname === "localhost" ||
  (controlPlaneURL.pathname !== "/" && controlPlaneURL.pathname !== "")
) {
  throw new Error("CONTROL_PLANE_URL must be a credential-free public HTTPS origin");
}
const canonicalControlPlaneURL = controlPlaneURL.toString();

let parsedRoutePublicKeys;
try {
  parsedRoutePublicKeys = JSON.parse(rawRoutePublicKeys);
} catch {
  throw new Error("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS must be a valid Ed25519 keyring");
}
if (!parsedRoutePublicKeys || typeof parsedRoutePublicKeys !== "object" ||
    Array.isArray(parsedRoutePublicKeys)) {
  throw new Error("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS must be a valid Ed25519 keyring");
}
const routePublicKeyEntries = Object.entries(parsedRoutePublicKeys)
  .sort(([left], [right]) => left < right ? -1 : left > right ? 1 : 0);
if (routePublicKeyEntries.length < 1 || routePublicKeyEntries.length > 4 ||
    routePublicKeyEntries.some(([keyID, encoded]) => {
      if (!/^[a-z][a-z0-9_-]{0,63}$/.test(keyID) ||
          typeof encoded !== "string" ||
          !/^[A-Za-z0-9+/]{43}=$/.test(encoded)) return true;
      const decoded = Buffer.from(encoded, "base64");
      return decoded.byteLength !== 32 || decoded.toString("base64") !== encoded;
    })) {
  throw new Error("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS must be a valid Ed25519 keyring");
}
const canonicalRoutePublicKeys = JSON.stringify(
  Object.fromEntries(routePublicKeyEntries),
);

// Defense in depth against accidentally pasting the broad control-plane KV id.
// The route manager also requires the dedicated namespace's exact title.
const controlPlaneConfig = await readFile(join(root, "../control-plane/wrangler.template.jsonc"), "utf8");
const controlPlaneDirectory = /"binding"\s*:\s*"DIRECTORY"[\s\S]{0,200}?"id"\s*:\s*"([0-9a-f]{32})"/.exec(controlPlaneConfig);
if (!controlPlaneDirectory) throw new Error("could not identify the control-plane DIRECTORY binding");
if (namespaceID === controlPlaneDirectory[1]) {
  throw new Error("EMAIL_DIRECTORY_KV_ID must not reuse the control-plane DIRECTORY namespace");
}

const template = await readFile(join(root, "wrangler.template.jsonc"), "utf8");
if ((template.match(/__EMAIL_DIRECTORY_KV_ID__/g) ?? []).length !== 1 ||
    (template.match(/__RELAY_KEY_ID__/g) ?? []).length !== 1 ||
    (template.match(/__CONTROL_PLANE_URL__/g) ?? []).length !== 1 ||
    (template.match(/__AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS__/g) ?? []).length !== 1 ||
    (template.match(/__AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST__/g) ?? []).length !== 1 ||
    (template.match(/__WITSELF_EDGE_RELEASE_VERSION__/g) ?? []).length !== 1 ||
    (template.match(/__WITSELF_EDGE_RELEASE_COMMIT__/g) ?? []).length !== 1 ||
    (template.match(/__WITSELF_EDGE_RELEASE_DATE__/g) ?? []).length !== 1 ||
    (template.match(/__REALM_EMAIL_ALIAS_DELIVERY_ENABLED__/g) ?? []).length !== 1 ||
    (template.match(/__REALM_EMAIL_CANONICAL_DELIVERY_ENABLED__/g) ?? []).length !== 1 ||
    (template.match(/__AGENT_EMAIL_DMARC_REJECT_ENABLED__/g) ?? []).length !== 1 ||
    (template.match(/__AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID__/g) ?? []).length !== 1 ||
    (template.match(/__AGENT_EMAIL_RELAY_VERSION__/g) ?? []).length !== 1) {
  throw new Error("wrangler template placeholders are invalid");
}
const rendered = template
  .replace("__EMAIL_DIRECTORY_KV_ID__", namespaceID)
  .replace("__RELAY_KEY_ID__", keyID)
  .replace("__CONTROL_PLANE_URL__", canonicalControlPlaneURL)
  .replace(
    "__AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS__",
    JSON.stringify(canonicalRoutePublicKeys),
  )
  .replace(
    "__AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST__",
    managedDeliveryAccountAllowlist,
  )
  .replace("__WITSELF_EDGE_RELEASE_VERSION__", release.version)
  .replace("__WITSELF_EDGE_RELEASE_COMMIT__", release.commit)
  .replace("__WITSELF_EDGE_RELEASE_DATE__", release.date)
  .replace(
    "__REALM_EMAIL_ALIAS_DELIVERY_ENABLED__",
    aliasDeliveryEnabled,
  )
  .replace(
    "__REALM_EMAIL_CANONICAL_DELIVERY_ENABLED__",
    canonicalDeliveryEnabled,
  )
  .replace(
    "__AGENT_EMAIL_DMARC_REJECT_ENABLED__",
    dmarcRejectEnabled,
  )
  .replace(
    "__AGENT_EMAIL_AUTH_RESULTS_AUTHSERV_ID__",
    JSON.stringify(authenticationResultsAuthservID),
  )
  .replace(
    "__AGENT_EMAIL_RELAY_VERSION__",
    relayVersion,
  );
await writeFile(renderedOutput, rendered, { mode: 0o600 });
process.stdout.write("rendered isolated email Worker configuration\n");
