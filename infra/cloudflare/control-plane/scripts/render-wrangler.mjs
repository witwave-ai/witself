#!/usr/bin/env node
import { chmod, readFile, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  sourceIdentity,
  validateBuildMetadata,
} from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i += 1) {
    const name = argv[i];
    if (!["--version", "--commit", "--date", "--output"].includes(name)) {
      throw new Error(`unknown argument ${name}`);
    }
    const value = argv[++i];
    if (!value) throw new Error(`${name} requires a value`);
    out[name.slice(2)] = value;
  }
  const supplied = ["version", "commit", "date"].filter((name) => out[name] != null);
  if (supplied.length !== 0 && supplied.length !== 3) {
    throw new Error("--version, --commit, and --date must be supplied together");
  }
  return out;
}

function releaseMetadata(args) {
  if (args.version != null) {
    return validateBuildMetadata({
      version: args.version,
      commit: args.commit,
      date: args.date,
    });
  }
  return sourceIdentity();
}

const args = parseArgs(process.argv.slice(2));
const metadata = releaseMetadata(args);
const emailDirectoryID = String(process.env.EMAIL_DIRECTORY_KV_ID ?? "");
if (!/^[0-9a-f]{32}$/.test(emailDirectoryID)) {
  throw new Error(
    "EMAIL_DIRECTORY_KV_ID must be the dedicated 32-character lowercase hex agent-email namespace id",
  );
}
const routeSigningKeyID = String(
  process.env.AGENT_EMAIL_ROUTE_SIGNING_KEY_ID ?? "",
);
if (!/^[a-z][a-z0-9_-]{0,63}$/.test(routeSigningKeyID)) {
  throw new Error(
    "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID must identify the active route signing key",
  );
}

const template = await readFile(join(root, "wrangler.template.jsonc"), "utf8");
const controlPlaneDirectory =
  /"binding"\s*:\s*"DIRECTORY"[\s\S]{0,200}?"id"\s*:\s*"([0-9a-f]{32})"/
    .exec(template);
if (!controlPlaneDirectory || emailDirectoryID === controlPlaneDirectory[1]) {
  throw new Error(
    "EMAIL_DIRECTORY_KV_ID must not reuse the control-plane DIRECTORY namespace",
  );
}
const replacements = new Map([
  ["__WITSELF_VERSION__", metadata.version],
  ["__WITSELF_COMMIT__", metadata.commit],
  ["__WITSELF_DATE__", metadata.date],
  ["__WITSELF_EDGE_RELEASE_VERSION__", metadata.version],
  ["__WITSELF_EDGE_RELEASE_COMMIT__", metadata.commit],
  ["__WITSELF_EDGE_RELEASE_DATE__", metadata.date],
  ["__EMAIL_DIRECTORY_KV_ID__", emailDirectoryID],
  ["__AGENT_EMAIL_ROUTE_SIGNING_KEY_ID__", routeSigningKeyID],
]);
let rendered = template;
for (const [placeholder, value] of replacements) {
  if ((rendered.match(new RegExp(placeholder, "g")) ?? []).length !== 1) {
    throw new Error(`wrangler template must contain ${placeholder} exactly once`);
  }
  rendered = rendered.replace(placeholder, value);
}
if (/__[A-Z][A-Z_]+__/.test(rendered)) {
  throw new Error("wrangler template contains an unresolved placeholder");
}

const output = args.output == null
  ? join(root, "wrangler.generated.jsonc")
  : isAbsolute(args.output) ? args.output : resolve(root, args.output);
await writeFile(output, rendered, { mode: 0o600 });
await chmod(output, 0o600);
process.stdout.write(
  `rendered control-plane release ${metadata.version} (${metadata.commit})\n`,
);
