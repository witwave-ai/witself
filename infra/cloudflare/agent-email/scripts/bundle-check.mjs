#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, dirname, isAbsolute, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { sourceIdentity } from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    ...options,
  });
  if (result.error || result.status !== 0) {
    const detail = String(result.stderr ?? "").trim().slice(-1000);
    throw new Error(`email Worker bundle check failed${detail ? `: ${detail}` : ""}`);
  }
  return String(result.stdout ?? "").trim();
}

async function filesBelow(directory) {
  const result = [];
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) result.push(...await filesBelow(path));
    else if (entry.isFile()) result.push(path);
  }
  return result.sort();
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value).sort().map((key) => [key, canonicalize(value[key])]),
    );
  }
  return value;
}

function parseObject(raw, label) {
  let value;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error(`${label} was not valid JSON`);
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} was not a JSON object`);
  }
  return value;
}

export function normalizedSourceMap(raw, {
  repositoryRoot = resolve(root, "..", "..", ".."),
  outputRoot,
} = {}) {
  const value = parseObject(raw, "Worker source map");
  if (!outputRoot || !Array.isArray(value.sources) ||
      !Array.isArray(value.sourcesContent) ||
      value.sources.length !== value.sourcesContent.length ||
      typeof value.sourceRoot !== "string") {
    throw new Error("Worker source map contract was invalid");
  }
  const sourceRoot = isAbsolute(value.sourceRoot)
    ? value.sourceRoot
    : resolve(outputRoot, value.sourceRoot);
  const sources = value.sources.map((source) => {
    if (typeof source !== "string" || source === "") {
      throw new Error("Worker source map contract was invalid");
    }
    const repositoryPath = relative(repositoryRoot, resolve(sourceRoot, source));
    if (repositoryPath === "" || repositoryPath === ".." ||
        repositoryPath.startsWith(`..${process.platform === "win32" ? "\\" : "/"}`) ||
        isAbsolute(repositoryPath)) {
      throw new Error("Worker source map referenced a file outside the repository");
    }
    return repositoryPath.replaceAll("\\", "/");
  });
  return JSON.stringify(canonicalize({
    ...value,
    sourceRoot: ".",
    sources,
  }));
}

export function normalizedMetafile(raw) {
  const value = parseObject(raw, "Worker bundle metafile");
  if (!value.outputs || typeof value.outputs !== "object" ||
      Array.isArray(value.outputs)) {
    throw new Error("Worker bundle metafile contract was invalid");
  }
  const outputs = {};
  for (const [path, metadata] of Object.entries(value.outputs)) {
    const name = basename(path);
    if (!name || Object.hasOwn(outputs, name)) {
      throw new Error("Worker bundle metafile output inventory was invalid");
    }
    outputs[name] = metadata;
  }
  return JSON.stringify(canonicalize({ ...value, outputs }));
}

async function bundleDigest(directory) {
  const files = (await filesBelow(directory)).map((path) =>
    relative(directory, path).replaceAll("\\", "/"));
  if (JSON.stringify(files) !==
      JSON.stringify(["README.md", "index.js", "index.js.map"])) {
    throw new Error("Worker dry-run output inventory was unexpected");
  }
  const digest = createHash("sha256");
  digest.update("index.js\0");
  digest.update(await readFile(join(directory, "index.js")));
  digest.update("\0index.js.map\0");
  digest.update(normalizedSourceMap(
    await readFile(join(directory, "index.js.map"), "utf8"),
    { outputRoot: directory },
  ));
  digest.update("\0");
  return digest.digest("hex");
}

export async function main() {
  const temporary = await mkdtemp(join(tmpdir(), "witself-agent-email-bundle-"));
  try {
    const config = join(temporary, "wrangler.generated.jsonc");
    const output = join(temporary, "bundle");
    const metafile = join(temporary, "bundle-meta.json");
    const controlPlane = await readFile(
      join(root, "..", "control-plane", "wrangler.template.jsonc"),
      "utf8",
    );
    const controlPlaneDirectory = /"binding"\s*:\s*"DIRECTORY"[\s\S]{0,200}?"id"\s*:\s*"([0-9a-f]{32})"/.exec(controlPlane)?.[1];
    if (!controlPlaneDirectory) throw new Error("could not identify control-plane directory binding");
    const directoryID = controlPlaneDirectory === "a".repeat(32)
      ? "b".repeat(32)
      : "a".repeat(32);
    const env = {
      ...process.env,
      EMAIL_DIRECTORY_KV_ID: directoryID,
      RELAY_KEY_ID: "bundle-check",
      CONTROL_PLANE_URL: "https://self.witwave.ai/",
      AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS:
        JSON.stringify({ "bundle-check": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" }),
      AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST: "",
      REALM_EMAIL_ALIAS_DELIVERY_ENABLED: "false",
      REALM_EMAIL_CANONICAL_DELIVERY_ENABLED: "false",
    };
    run(process.execPath, [
      join(root, "scripts", "render-wrangler.mjs"),
      "--output", config,
    ], { env });
    run("wrangler", [
      "deploy",
      join(root, "src", "index.js"),
      "--dry-run",
      "--config", config,
      "--outdir", output,
      "--metafile", metafile,
    ], { env });
    const source = sourceIdentity();
    const wranglerVersion = run("wrangler", ["--version"]);
    const manifest = {
      schema: "witself.agent-email-edge-bundle.v1",
      outcome: "verified",
      source: {
        version: source.version,
        commit: source.commit,
        date: source.date,
        clean: source.clean,
      },
      wrangler_version: wranglerVersion,
      bundle_sha256: await bundleDigest(output),
      metafile_sha256: createHash("sha256")
        .update(normalizedMetafile(await readFile(metafile, "utf8")))
        .digest("hex"),
    };
    process.stdout.write(`${JSON.stringify(manifest, null, 2)}\n`);
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
