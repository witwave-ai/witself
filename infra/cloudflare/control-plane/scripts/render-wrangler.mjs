#!/usr/bin/env node
import { execFileSync } from "node:child_process";
import { chmod, readFile, writeFile } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const repoRoot = resolve(root, "../../..");

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

function git(...args) {
  return execFileSync("git", ["-C", repoRoot, ...args], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

function releaseMetadata(args) {
  if (args.version != null) {
    return {
      version: args.version,
      commit: args.commit,
      date: args.date,
    };
  }

  const dirty = git("status", "--porcelain");
  if (dirty !== "") {
    throw new Error("control-plane deployment requires a clean checkout");
  }
  const releaseTags = git("tag", "--points-at", "HEAD", "--list", "v*")
    .split("\n")
    .filter((tag) => /^v[0-9]+\.[0-9]+\.[0-9]+$/.test(tag));
  if (releaseTags.length !== 1) {
    throw new Error(
      "control-plane deployment requires HEAD to have exactly one vMAJOR.MINOR.PATCH release tag",
    );
  }
  return {
    version: releaseTags[0].slice(1),
    commit: git("rev-parse", "HEAD"),
    date: git("show", "--no-show-signature", "-s", "--format=%cI", "HEAD"),
  };
}

function validateMetadata(metadata) {
  if (!/^[0-9]+\.[0-9]+\.[0-9]+$/.test(metadata.version)) {
    throw new Error("version must be MAJOR.MINOR.PATCH without a v prefix");
  }
  if (!/^[0-9a-f]{40}$/.test(metadata.commit)) {
    throw new Error("commit must be a full lowercase Git SHA");
  }
  if (
    !/^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:Z|[+-][0-9]{2}:[0-9]{2})$/
      .test(metadata.date) ||
    Number.isNaN(Date.parse(metadata.date))
  ) {
    throw new Error("date must be an RFC3339 timestamp");
  }
}

const args = parseArgs(process.argv.slice(2));
const metadata = releaseMetadata(args);
validateMetadata(metadata);

const template = await readFile(join(root, "wrangler.template.jsonc"), "utf8");
const replacements = new Map([
  ["__WITSELF_VERSION__", metadata.version],
  ["__WITSELF_COMMIT__", metadata.commit],
  ["__WITSELF_DATE__", metadata.date],
]);
let rendered = template;
for (const [placeholder, value] of replacements) {
  if ((rendered.match(new RegExp(placeholder, "g")) ?? []).length !== 1) {
    throw new Error(`wrangler template must contain ${placeholder} exactly once`);
  }
  rendered = rendered.replace(placeholder, value);
}
if (/__WITSELF_[A-Z_]+__/.test(rendered)) {
  throw new Error("wrangler template contains an unresolved build placeholder");
}

const output = args.output == null
  ? join(root, "wrangler.generated.jsonc")
  : isAbsolute(args.output) ? args.output : resolve(root, args.output);
await writeFile(output, rendered, { mode: 0o600 });
await chmod(output, 0o600);
process.stdout.write(
  `rendered control-plane release ${metadata.version} (${metadata.commit})\n`,
);
