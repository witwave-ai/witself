#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const namespaceID = String(process.env.EMAIL_DIRECTORY_KV_ID ?? "");
const keyID = String(process.env.RELAY_KEY_ID ?? "").trim().toLowerCase();
const rawControlPlaneURL = String(process.env.CONTROL_PLANE_URL ?? "");
const aliasDeliveryEnabled = String(
  process.env.REALM_EMAIL_ALIAS_DELIVERY_ENABLED ?? "false",
);
const canonicalDeliveryEnabled = String(
  process.env.REALM_EMAIL_CANONICAL_DELIVERY_ENABLED ?? "false",
);
if (!/^[0-9a-f]{32}$/.test(namespaceID)) throw new Error("EMAIL_DIRECTORY_KV_ID must be a 32-character lowercase hex id");
if (!/^[a-z][a-z0-9_-]{0,63}$/.test(keyID)) throw new Error("RELAY_KEY_ID is missing or invalid");
if (!["true", "false"].includes(aliasDeliveryEnabled)) {
  throw new Error("REALM_EMAIL_ALIAS_DELIVERY_ENABLED must be true or false");
}
if (!["true", "false"].includes(canonicalDeliveryEnabled)) {
  throw new Error("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED must be true or false");
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
    (template.match(/__REALM_EMAIL_ALIAS_DELIVERY_ENABLED__/g) ?? []).length !== 1 ||
    (template.match(/__REALM_EMAIL_CANONICAL_DELIVERY_ENABLED__/g) ?? []).length !== 1) {
  throw new Error("wrangler template placeholders are invalid");
}
const rendered = template
  .replace("__EMAIL_DIRECTORY_KV_ID__", namespaceID)
  .replace("__RELAY_KEY_ID__", keyID)
  .replace("__CONTROL_PLANE_URL__", canonicalControlPlaneURL)
  .replace(
    "__REALM_EMAIL_ALIAS_DELIVERY_ENABLED__",
    aliasDeliveryEnabled,
  )
  .replace(
    "__REALM_EMAIL_CANONICAL_DELIVERY_ENABLED__",
    canonicalDeliveryEnabled,
  );
await writeFile(join(root, "wrangler.generated.jsonc"), rendered, { mode: 0o600 });
process.stdout.write("rendered isolated email Worker configuration\n");
