#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmod,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { devNull, tmpdir } from "node:os";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { assertExactConfig } from "./bundle-check.mjs";
import {
  sanitizedGitEnvironment,
  sourceIdentity,
} from "./source-identity.mjs";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const PRODUCTION_ACCOUNT_ID = "8f0bf04a4e7aab3a8cc60f02cc8c8fdb";
const SOURCE_PATH_PREFIX = "infra/cloudflare/support-email-intake/src/";
const SOURCE_FILES = Object.freeze([
  "authenticity.mjs",
  "index.js",
  "intake.mjs",
  "mime-text.mjs",
]);
const MAX_SOURCE_BYTES = 1024 * 1024;
const PRIVATE_DIRECTORY_MODE = 0o700;
const IMMUTABLE_FILE_MODE = 0o400;
const FULL_COMMIT = /^[0-9a-f]{40}$/;

function parseJSONC(raw) {
  return JSON.parse(raw.replace(/^\s*\/\/.*$/gm, ""));
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

export function runGitInspection(args, {
  checkout = resolve(root, "../../.."),
  environment = process.env,
} = {}) {
  return spawnSync("git", args, {
    cwd: checkout,
    env: sanitizedGitEnvironment(environment),
    encoding: null,
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: MAX_SOURCE_BYTES * 2,
    timeout: 30_000,
  });
}

function gitOutput(args, label, inspect) {
  const result = inspect(args);
  if (result?.error || result?.status !== 0 || !Buffer.isBuffer(result?.stdout)) {
    throw new Error(`could not freeze ${label} from the tagged release`);
  }
  return result.stdout;
}

// snapshotReleaseWorkerSource freezes the exact tagged Worker modules into a
// private read-only directory, removing the live worktree from the deploy path.
export async function snapshotReleaseWorkerSource(
  destinationRoot,
  commit,
  inspect = runGitInspection,
) {
  if (typeof destinationRoot !== "string" || !isAbsolute(destinationRoot) ||
      resolve(destinationRoot) !== destinationRoot || !FULL_COMMIT.test(commit) ||
      typeof inspect !== "function") {
    throw new Error("support email intake source snapshot request was invalid");
  }
  const directoryMetadata = await lstat(destinationRoot);
  if (!directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
      (directoryMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE) {
    throw new Error("support email intake source snapshot directory was unsafe");
  }
  const inventory = gitOutput([
    "ls-tree", "-rz", "--full-tree", commit, "--", SOURCE_PATH_PREFIX,
  ], "Worker source inventory", inspect).toString("utf8");
  const records = inventory.split("\0").filter(Boolean);
  const parsed = records.map((record) => {
    const match = /^(100644|100755) blob ([0-9a-f]{40,64})\t(.+)$/.exec(record);
    if (!match || !match[3].startsWith(SOURCE_PATH_PREFIX)) {
      throw new Error("tagged support email intake source inventory was invalid");
    }
    const path = match[3].slice(SOURCE_PATH_PREFIX.length);
    if (path.includes("/") || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(path) ||
        !/\.(?:js|mjs)$/.test(path)) {
      throw new Error("tagged support email intake source inventory was invalid");
    }
    return Object.freeze({ oid: match[2], path });
  }).sort((left, right) => left.path.localeCompare(right.path));
  if (JSON.stringify(parsed.map((item) => item.path)) !==
      JSON.stringify(SOURCE_FILES)) {
    throw new Error("tagged support email intake source inventory was invalid");
  }

  let totalBytes = 0;
  const manifest = [];
  for (const item of parsed) {
    const bytes = gitOutput(
      ["cat-file", "blob", item.oid],
      `Worker source ${item.path}`,
      inspect,
    );
    totalBytes += bytes.byteLength;
    if (bytes.byteLength < 1 || totalBytes > MAX_SOURCE_BYTES) {
      throw new Error("tagged support email intake source exceeded its size limit");
    }
    const target = join(destinationRoot, item.path);
    await writeFile(target, bytes, { flag: "wx", mode: IMMUTABLE_FILE_MODE });
    await chmod(target, IMMUTABLE_FILE_MODE);
    manifest.push(Object.freeze({
      path: item.path,
      bytes: bytes.byteLength,
      sha256: sha256(bytes),
    }));
  }

  const assertUnchanged = async () => {
    const [metadata, names] = await Promise.all([
      lstat(destinationRoot),
      readdir(destinationRoot),
    ]);
    if (!metadata.isDirectory() || metadata.isSymbolicLink() ||
        (metadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
        JSON.stringify(names.sort()) !== JSON.stringify([...SOURCE_FILES].sort())) {
      throw new Error("support email intake source snapshot changed during deployment");
    }
    for (const item of manifest) {
      const path = join(destinationRoot, item.path);
      const [fileMetadata, bytes] = await Promise.all([lstat(path), readFile(path)]);
      if (!fileMetadata.isFile() || fileMetadata.isSymbolicLink() ||
          (fileMetadata.mode & 0o777) !== IMMUTABLE_FILE_MODE ||
          bytes.byteLength !== item.bytes || sha256(bytes) !== item.sha256) {
        throw new Error("support email intake source snapshot changed during deployment");
      }
    }
  };
  await assertUnchanged();
  return Object.freeze({
    entrypoint: join(destinationRoot, "index.js"),
    file_count: manifest.length,
    byte_count: totalBytes,
    sha256: sha256(JSON.stringify(manifest)),
    assertUnchanged,
  });
}

export function assertReleaseUnchanged(expected, current) {
  if (!expected || !current || expected.version !== current.version ||
      expected.commit !== current.commit || expected.date !== current.date ||
      expected.tag !== current.tag || expected.clean !== true ||
      current.clean !== true) {
    throw new Error("support email intake release source changed during deployment");
  }
  return true;
}

export function productionDeploymentArguments(entrypoint, configPath, release) {
  if (typeof entrypoint !== "string" || !isAbsolute(entrypoint) ||
      typeof configPath !== "string" || !isAbsolute(configPath) ||
      !release || typeof release.tag !== "string" || !release.tag) {
    throw new Error("support email intake deployment arguments were invalid");
  }
  return [
    "deploy", entrypoint, "--config", configPath,
    "--tag", release.tag,
    "--message", `witself support email intake v${release.version} (${release.commit})`,
    "--env-file", devNull,
  ];
}

function deploymentEnvironment(source = process.env) {
  if (source.CLOUDFLARE_ACCOUNT_ID !== PRODUCTION_ACCOUNT_ID) {
    throw new Error(`CLOUDFLARE_ACCOUNT_ID must identify production account ${PRODUCTION_ACCOUNT_ID}`);
  }
  const apiToken = source.CLOUDFLARE_API_TOKEN;
  if (typeof apiToken !== "string" || apiToken.length < 1 ||
      apiToken.length > 4_096 || apiToken !== apiToken.trim() ||
      /[\s\0-\x1f\x7f]/u.test(apiToken)) {
    throw new Error("CLOUDFLARE_API_TOKEN is missing or invalid");
  }
  const allowed = [
    "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP",
    "LANG", "TZ", "CI", "GITHUB_ACTIONS", "CLOUDFLARE_ACCOUNT_ID",
    "CLOUDFLARE_API_TOKEN", "CONTROL_PLANE_URL",
    "SUPPORT_EMAIL_INTAKE_ENABLED",
    "SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID",
  ];
  return {
    ...Object.fromEntries(allowed.filter((name) => Object.hasOwn(source, name))
      .map((name) => [name, source[name]])),
    WRANGLER_WRITE_LOGS: "false",
    WRANGLER_LOG_SANITIZE: "true",
    WRANGLER_SEND_METRICS: "false",
    WRANGLER_SEND_ERROR_REPORTS: "false",
    NO_COLOR: "1",
    TERM: "dumb",
  };
}

function run(command, args, environment) {
  const result = spawnSync(command, args, {
    cwd: root,
    env: environment,
    encoding: "utf8",
    stdio: "inherit",
  });
  if (result.error || result.status !== 0) {
    throw new Error("support email intake Worker deployment failed");
  }
}

export async function main() {
  const release = sourceIdentity({ requireRelease: true });
  const environment = deploymentEnvironment();
  const temporary = await mkdtemp(join(tmpdir(), "witself-support-email-deploy-"));
  try {
    await chmod(temporary, PRIVATE_DIRECTORY_MODE);
    const sourceRoot = join(temporary, "source");
    await mkdir(sourceRoot, { mode: PRIVATE_DIRECTORY_MODE });
    const source = await snapshotReleaseWorkerSource(sourceRoot, release.commit);
    const configPath = join(temporary, "wrangler.generated.jsonc");
    run(process.execPath, [
      join(root, "scripts", "render-wrangler.mjs"), "--output", configPath,
    ], environment);
    const config = parseJSONC(await readFile(configPath, "utf8"));
    assertExactConfig(config);
    if (config.vars.WITSELF_EDGE_RELEASE_VERSION !== release.version ||
        config.vars.WITSELF_EDGE_RELEASE_COMMIT !== release.commit ||
        config.vars.WITSELF_EDGE_RELEASE_DATE !== release.date) {
      throw new Error("support email intake configuration release identity drifted");
    }
    assertReleaseUnchanged(release, sourceIdentity({ requireRelease: true }));
    await source.assertUnchanged();
    run(
      "wrangler",
      productionDeploymentArguments(source.entrypoint, configPath, release),
      environment,
    );
    await source.assertUnchanged();
    assertReleaseUnchanged(release, sourceIdentity({ requireRelease: true }));
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
