#!/usr/bin/env node
import { readFile, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { sourceIdentity } from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

function outputPath(argv) {
  if (argv.length === 0) return join(root, "wrangler.generated.jsonc");
  if (argv.length !== 2 || argv[0] !== "--output" || !argv[1]) {
    throw new Error("usage: render-wrangler.mjs [--output PATH]");
  }
  return resolve(root, argv[1]);
}

function publicControlPlaneURL(rawValue) {
  const raw = String(rawValue ?? "");
  let value;
  try {
    value = new URL(raw);
  } catch {
    throw new Error("CONTROL_PLANE_URL must be a credential-free public HTTPS origin");
  }
  if (
    raw !== raw.trim() || value.protocol !== "https:" || value.username ||
    value.password || value.search || value.hash || !value.hostname ||
    value.hostname === "localhost" ||
    (value.pathname !== "/" && value.pathname !== "")
  ) {
    throw new Error("CONTROL_PLANE_URL must be a credential-free public HTTPS origin");
  }
  return value.toString();
}

const enabled = String(process.env.SUPPORT_EMAIL_INTAKE_ENABLED ?? "false");
if (!["true", "false"].includes(enabled)) {
  throw new Error("SUPPORT_EMAIL_INTAKE_ENABLED must be true or false");
}
const authservID = String(
  process.env.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID ?? "",
);
if (authservID !== "" && !/^[\x21-\x3a\x3c-\x7e]{1,255}$/.test(authservID)) {
  throw new Error(
    "SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID must be empty or a printable non-space ASCII token without semicolons",
  );
}
const controlPlaneURL = publicControlPlaneURL(process.env.CONTROL_PLANE_URL);
const release = sourceIdentity();
const template = await readFile(join(root, "wrangler.template.jsonc"), "utf8");
const replacements = new Map([
  ["__SUPPORT_EMAIL_INTAKE_ENABLED__", enabled],
  ["__SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID__", JSON.stringify(authservID)],
  ["__CONTROL_PLANE_URL__", controlPlaneURL],
  ["__WITSELF_EDGE_RELEASE_VERSION__", release.version],
  ["__WITSELF_EDGE_RELEASE_COMMIT__", release.commit],
  ["__WITSELF_EDGE_RELEASE_DATE__", release.date],
]);
let rendered = template;
for (const [placeholder, value] of replacements) {
  if ((rendered.split(placeholder).length - 1) !== 1) {
    throw new Error("wrangler template placeholders are invalid");
  }
  rendered = rendered.replace(placeholder, value);
}
await writeFile(outputPath(process.argv.slice(2)), rendered, { mode: 0o600 });
process.stdout.write("rendered support email intake Worker configuration\n");
