#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const namespaceID = String(process.env.EMAIL_DIRECTORY_KV_ID ?? "");
const keyID = String(process.env.RELAY_KEY_ID ?? "").trim().toLowerCase();
if (!/^[0-9a-f]{32}$/.test(namespaceID)) throw new Error("EMAIL_DIRECTORY_KV_ID must be a 32-character lowercase hex id");
if (!/^[a-z][a-z0-9_-]{0,63}$/.test(keyID)) throw new Error("RELAY_KEY_ID is missing or invalid");

// Defense in depth against accidentally pasting the broad control-plane KV id.
// The route manager also requires the dedicated namespace's exact title.
const controlPlaneConfig = await readFile(join(root, "../control-plane/wrangler.jsonc"), "utf8");
const controlPlaneDirectory = /"binding"\s*:\s*"DIRECTORY"[\s\S]{0,200}?"id"\s*:\s*"([0-9a-f]{32})"/.exec(controlPlaneConfig);
if (!controlPlaneDirectory) throw new Error("could not identify the control-plane DIRECTORY binding");
if (namespaceID === controlPlaneDirectory[1]) {
  throw new Error("EMAIL_DIRECTORY_KV_ID must not reuse the control-plane DIRECTORY namespace");
}

const template = await readFile(join(root, "wrangler.template.jsonc"), "utf8");
if ((template.match(/__EMAIL_DIRECTORY_KV_ID__/g) ?? []).length !== 1 ||
    (template.match(/__RELAY_KEY_ID__/g) ?? []).length !== 1) {
  throw new Error("wrangler template placeholders are invalid");
}
const rendered = template
  .replace("__EMAIL_DIRECTORY_KV_ID__", namespaceID)
  .replace("__RELAY_KEY_ID__", keyID);
await writeFile(join(root, "wrangler.generated.jsonc"), rendered, { mode: 0o600 });
process.stdout.write("rendered isolated email Worker configuration\n");
