import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmod,
  cp,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  readdir,
  realpath,
  rm,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import {
  dirname,
  isAbsolute,
  join,
  relative,
  resolve,
  sep,
} from "node:path";
import { fileURLToPath } from "node:url";

import { createPrivateDeploymentConfig } from
  "./private-deployment-config.mjs";
import { assertReleaseSource } from "./source-identity.mjs";

const controlPlaneRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const defaultRepositoryRoot = resolve(controlPlaneRoot, "../../..");
const defaultRuntimeDependencyRoot = join(
  controlPlaneRoot,
  "node_modules",
  "@cloudflare",
  "containers",
);

const SNAPSHOT_PREFIX = "witself-control-plane-release-";
const PRIVATE_CONFIG_PREFIX = "witself-control-plane-deploy-";
const PRIVATE_DIRECTORY_MODE = 0o700;
const FROZEN_DIRECTORY_MODE = 0o555;
const FROZEN_FILE_MODE = 0o444;
const FROZEN_EXECUTABLE_MODE = 0o555;
const MAX_SOURCE_FILES = 2048;
const MAX_SOURCE_BYTES = 64 * 1024 * 1024;
const MAX_SOURCE_FILE_BYTES = 16 * 1024 * 1024;
const MAX_DEPENDENCY_FILES = 64;
const MAX_DEPENDENCY_BYTES = 2 * 1024 * 1024;
const MAX_PATH_BYTES = 512;
const MAX_PATH_DEPTH = 16;
const REVIEWED_ENV_FILE_CONTENT =
  "# Intentionally empty: production Wrangler commands must not load local dotenv files.\n";

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function exactPathInside(parent, child) {
  const path = relative(parent, child);
  return path !== "" && !isAbsolute(path) &&
    path !== ".." && !path.startsWith(`..${sep}`);
}

function safeInventoryPath(path) {
  if (typeof path !== "string" || path === "" ||
      Buffer.byteLength(path) > MAX_PATH_BYTES ||
      path.includes("\\") || /[\x00-\x1f\x7f]/.test(path)) {
    return false;
  }
  const components = path.split("/");
  return components.length <= MAX_PATH_DEPTH && components.every((part) =>
    part !== "" && part !== "." && part !== ".." &&
    /^[A-Za-z0-9_@+.-]{1,128}$/.test(part));
}

function safeGitEnvironment(source = process.env) {
  const environment = { ...source };
  for (const name of Object.keys(environment)) {
    if (name.startsWith("GIT_") || name === "SSH_ASKPASS" ||
        name === "GIT_ASKPASS") {
      delete environment[name];
    }
  }
  Object.assign(environment, {
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_TERMINAL_PROMPT: "0",
  });
  return environment;
}

function runGit(repositoryRoot, args, maxBuffer = MAX_SOURCE_BYTES * 2) {
  return spawnSync("git", args, {
    cwd: repositoryRoot,
    env: safeGitEnvironment(),
    encoding: null,
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer,
    timeout: 60_000,
  });
}

function requiredOutput(result, operation) {
  if (result?.error || result?.status !== 0 ||
      !Buffer.isBuffer(result?.stdout)) {
    throw new Error(`could not ${operation} from the tagged release`);
  }
  return result.stdout;
}

function parseTaggedTree(bytes) {
  const records = bytes.subarray(0, bytes.length - (bytes.at(-1) === 0 ? 1 : 0))
    .toString("utf8")
    .split("\0")
    .filter(Boolean);
  if (records.length < 1 || records.length > MAX_SOURCE_FILES) {
    throw new Error("tagged control-plane repository inventory was outside its file limit");
  }
  let byteCount = 0;
  const entries = records.map((record) => {
    const match = /^(100644|100755) blob ([0-9a-f]{40}|[0-9a-f]{64}) +([0-9]+)\t(.+)$/.exec(
      record,
    );
    if (!match || !safeInventoryPath(match[4])) {
      throw new Error("tagged control-plane repository inventory was invalid");
    }
    const size = Number(match[3]);
    if (!Number.isSafeInteger(size) || size < 0 ||
        size > MAX_SOURCE_FILE_BYTES) {
      throw new Error("tagged control-plane repository file was outside its size limit");
    }
    byteCount += size;
    if (byteCount > MAX_SOURCE_BYTES) {
      throw new Error("tagged control-plane repository exceeded its byte limit");
    }
    return Object.freeze({
      path: match[4],
      mode: Number.parseInt(match[1].slice(3), 8),
      oid: match[2],
      bytes: size,
    });
  }).sort((left, right) => left.path.localeCompare(right.path));
  if (new Set(entries.map((entry) => entry.path)).size !== entries.length ||
      !entries.some((entry) =>
        entry.path === "infra/cloudflare/control-plane/src/index.js") ||
      !entries.some((entry) =>
        entry.path === "images/witself-control-plane/Dockerfile") ||
      !entries.some((entry) => entry.path === ".dockerignore")) {
    throw new Error("tagged control-plane repository inventory was incomplete");
  }
  return Object.freeze({ entries: Object.freeze(entries), byteCount });
}

function gitBlobOID(bytes, expectedOID) {
  const algorithm = expectedOID.length === 64 ? "sha256" : "sha1";
  const prefix = Buffer.from(`blob ${bytes.byteLength}\0`);
  return createHash(algorithm).update(prefix).update(bytes).digest("hex");
}

async function scanTree(
  root,
  {
    maxFiles,
    maxBytes,
    maxFileBytes,
    ignoredDirectory = "",
  },
) {
  const entries = [];
  let fileCount = 0;
  let byteCount = 0;

  const rootMetadata = await lstat(root);
  if (!rootMetadata.isDirectory() || rootMetadata.isSymbolicLink()) {
    throw new Error("control-plane release snapshot root was unsafe");
  }

  const visit = async (directory, prefix = "") => {
    const names = await readdir(directory, { withFileTypes: true });
    names.sort((left, right) => left.name.localeCompare(right.name));
    for (const item of names) {
      const path = prefix === "" ? item.name : `${prefix}/${item.name}`;
      if (!safeInventoryPath(path)) {
        throw new Error("control-plane release snapshot contained an unsafe path");
      }
      const absolute = join(directory, item.name);
      const metadata = await lstat(absolute);
      if (path === ignoredDirectory) {
        if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
          throw new Error("control-plane private config directory was unsafe");
        }
        continue;
      }
      const mode = metadata.mode & 0o777;
      if (metadata.isDirectory() && !metadata.isSymbolicLink()) {
        entries.push(Object.freeze({ path, kind: "directory", mode }));
        await visit(absolute, path);
        continue;
      }
      if (!metadata.isFile() || metadata.isSymbolicLink()) {
        throw new Error("control-plane release snapshot contained a non-file entry");
      }
      fileCount += 1;
      if (fileCount > maxFiles || metadata.size > maxFileBytes) {
        throw new Error("control-plane release snapshot exceeded its file limit");
      }
      byteCount += metadata.size;
      if (byteCount > maxBytes) {
        throw new Error("control-plane release snapshot exceeded its byte limit");
      }
      const bytes = await readFile(absolute);
      if (bytes.byteLength !== metadata.size) {
        throw new Error("control-plane release snapshot changed while it was read");
      }
      entries.push(Object.freeze({
        path,
        kind: "file",
        mode,
        bytes: bytes.byteLength,
        sha256: sha256(bytes),
      }));
    }
  };
  await visit(root);
  return Object.freeze({
    entries: Object.freeze(entries),
    file_count: fileCount,
    byte_count: byteCount,
    sha256: sha256(JSON.stringify(entries)),
  });
}

function sameInventory(left, right) {
  return left.file_count === right.file_count &&
    left.byte_count === right.byte_count &&
    left.sha256 === right.sha256 &&
    JSON.stringify(left.entries) === JSON.stringify(right.entries);
}

async function assertTaggedExtraction(repositoryRoot, expected) {
  const actual = await scanTree(repositoryRoot, {
    maxFiles: MAX_SOURCE_FILES,
    maxBytes: MAX_SOURCE_BYTES,
    maxFileBytes: MAX_SOURCE_FILE_BYTES,
  });
  const files = actual.entries
    .filter((entry) => entry.kind === "file")
    .sort((left, right) => left.path.localeCompare(right.path));
  if (files.length !== expected.entries.length) {
    throw new Error("tagged control-plane repository extraction was incomplete");
  }
  for (let index = 0; index < files.length; index += 1) {
    const actualEntry = files[index];
    const expectedEntry = expected.entries[index];
    if (actualEntry.path !== expectedEntry.path ||
        actualEntry.mode !== expectedEntry.mode ||
        actualEntry.bytes !== expectedEntry.bytes) {
      throw new Error("tagged control-plane repository extraction did not match Git");
    }
    const bytes = await readFile(join(repositoryRoot, actualEntry.path));
    if (gitBlobOID(bytes, expectedEntry.oid) !== expectedEntry.oid) {
      throw new Error("tagged control-plane repository blob did not match Git");
    }
  }
}

async function exportTaggedRepository(repositoryRoot, destination, identity) {
  const expected = parseTaggedTree(requiredOutput(
    runGit(repositoryRoot, [
      "ls-tree", "-rlz", "--full-tree", identity.commit,
    ]),
    "read the control-plane repository inventory",
  ));
  const archive = requiredOutput(
    runGit(
      repositoryRoot,
      ["archive", "--format=tar", identity.commit],
      MAX_SOURCE_BYTES + (16 * 1024 * 1024),
    ),
    "export the control-plane repository",
  );
  if (archive.byteLength < 1 ||
      archive.byteLength > MAX_SOURCE_BYTES + (16 * 1024 * 1024)) {
    throw new Error("tagged control-plane repository archive was outside its limit");
  }
  const extracted = spawnSync("tar", ["-x", "-f", "-", "-C", destination], {
    input: archive,
    encoding: null,
    stdio: ["pipe", "ignore", "pipe"],
    maxBuffer: 1024 * 1024,
    timeout: 60_000,
  });
  if (extracted.error || extracted.status !== 0) {
    throw new Error("could not extract the tagged control-plane repository");
  }
  await assertTaggedExtraction(destination, expected);
}

function comparableDependencyInventory(inventory) {
  return inventory.entries.map((entry) => ({
    path: entry.path,
    kind: entry.kind,
    bytes: entry.bytes,
    sha256: entry.sha256,
  }));
}

async function copyLockedRuntimeDependency(
  repositoryRoot,
  installedDependencyRoot,
) {
  const packageRoot = join(
    repositoryRoot,
    "infra",
    "cloudflare",
    "control-plane",
  );
  const [packageDefinition, lockDefinition, installedDefinition] =
    await Promise.all([
      readFile(join(packageRoot, "package.json"), "utf8").then(JSON.parse),
      readFile(join(packageRoot, "package-lock.json"), "utf8").then(JSON.parse),
      readFile(join(installedDependencyRoot, "package.json"), "utf8")
        .then(JSON.parse),
    ]);
  const requested = packageDefinition?.dependencies?.["@cloudflare/containers"];
  const locked = lockDefinition?.packages?.[
    "node_modules/@cloudflare/containers"
  ];
  if (requested !== `^${locked?.version}` ||
      locked?.version !== installedDefinition?.version ||
      installedDefinition?.name !== "@cloudflare/containers" ||
      !/^sha512-[A-Za-z0-9+/]+={0,2}$/.test(String(locked?.integrity ?? "")) ||
      locked?.resolved !==
        `https://registry.npmjs.org/@cloudflare/containers/-/containers-${locked.version}.tgz` ||
      JSON.stringify(installedDefinition.files) !== JSON.stringify(["dist"]) ||
      (installedDefinition.dependencies != null &&
       Object.keys(installedDefinition.dependencies).length !== 0)) {
    throw new Error(
      "installed @cloudflare/containers did not match the tagged production lock",
    );
  }
  const before = await scanTree(installedDependencyRoot, {
    maxFiles: MAX_DEPENDENCY_FILES,
    maxBytes: MAX_DEPENDENCY_BYTES,
    maxFileBytes: MAX_DEPENDENCY_BYTES,
  });
  const target = join(
    packageRoot,
    "node_modules",
    "@cloudflare",
    "containers",
  );
  await mkdir(dirname(target), { recursive: true, mode: PRIVATE_DIRECTORY_MODE });
  await cp(installedDependencyRoot, target, {
    recursive: true,
    errorOnExist: true,
    force: false,
    preserveTimestamps: false,
  });
  const [after, copied] = await Promise.all([
    scanTree(installedDependencyRoot, {
      maxFiles: MAX_DEPENDENCY_FILES,
      maxBytes: MAX_DEPENDENCY_BYTES,
      maxFileBytes: MAX_DEPENDENCY_BYTES,
    }),
    scanTree(target, {
      maxFiles: MAX_DEPENDENCY_FILES,
      maxBytes: MAX_DEPENDENCY_BYTES,
      maxFileBytes: MAX_DEPENDENCY_BYTES,
    }),
  ]);
  if (!sameInventory(before, after) ||
      JSON.stringify(comparableDependencyInventory(before)) !==
        JSON.stringify(comparableDependencyInventory(copied))) {
    throw new Error("installed @cloudflare/containers changed while it was copied");
  }
  return target;
}

async function freezeTree(root, ignoredDirectory = "", prefix = "") {
  const names = await readdir(root, { withFileTypes: true });
  names.sort((left, right) => left.name.localeCompare(right.name));
  for (const item of names) {
    const path = prefix === "" ? item.name : `${prefix}/${item.name}`;
    const absolute = join(root, item.name);
    const metadata = await lstat(absolute);
    if (path === ignoredDirectory) {
      if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
        throw new Error("control-plane private config directory was unsafe");
      }
      continue;
    }
    if (metadata.isDirectory() && !metadata.isSymbolicLink()) {
      await freezeTree(absolute, ignoredDirectory, path);
      await chmod(absolute, FROZEN_DIRECTORY_MODE);
    } else if (metadata.isFile() && !metadata.isSymbolicLink()) {
      const mode = metadata.mode & 0o111
        ? FROZEN_EXECUTABLE_MODE
        : FROZEN_FILE_MODE;
      await chmod(absolute, mode);
    } else {
      throw new Error("control-plane release snapshot contained a non-file entry");
    }
  }
  if (prefix === "") await chmod(root, FROZEN_DIRECTORY_MODE);
}

async function thawDirectories(root) {
  let metadata;
  try {
    metadata = await lstat(root);
  } catch {
    return;
  }
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) return;
  await chmod(root, PRIVATE_DIRECTORY_MODE).catch(() => {});
  const names = await readdir(root, { withFileTypes: true }).catch(() => []);
  for (const item of names) {
    if (item.isDirectory() && !item.isSymbolicLink()) {
      await thawDirectories(join(root, item.name));
    }
  }
}

function assertDockerContextExclusions(source) {
  const lines = source.split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("#"));
  for (const required of [
    "node_modules",
    "infra/cloudflare/control-plane/node_modules",
    "infra/cloudflare/witself-control-plane-deploy-*/",
  ]) {
    if (!lines.includes(required)) {
      throw new Error(
        `tagged Docker context did not exclude release-only input ${required}`,
      );
    }
  }
}

export async function createControlPlaneReleaseSnapshot({
  identity,
  render,
  validate,
  repositoryRoot = defaultRepositoryRoot,
  installedDependencyRoot = defaultRuntimeDependencyRoot,
  exportRepository = exportTaggedRepository,
}) {
  const release = assertReleaseSource(identity);
  if (typeof render !== "function" ||
      (validate != null && typeof validate !== "function") ||
      typeof exportRepository !== "function" ||
      !isAbsolute(repositoryRoot) || resolve(repositoryRoot) !== repositoryRoot ||
      !isAbsolute(installedDependencyRoot) ||
      resolve(installedDependencyRoot) !== installedDependencyRoot) {
    throw new Error("control-plane release snapshot options were invalid");
  }
  const [exactRepositoryRoot, exactTemporaryRoot] = await Promise.all([
    realpath(repositoryRoot),
    realpath(tmpdir()),
  ]);
  if (exactPathInside(exactRepositoryRoot, exactTemporaryRoot) ||
      exactRepositoryRoot === exactTemporaryRoot) {
    throw new Error("control-plane release snapshot root must be outside the checkout");
  }

  const directory = await mkdtemp(join(exactTemporaryRoot, SNAPSHOT_PREFIX));
  const snapshotRepository = join(directory, "repository");
  const workDirectory = join(directory, "work");
  let config;
  let cleaned = false;
  try {
    await chmod(directory, PRIVATE_DIRECTORY_MODE);
    await mkdir(snapshotRepository, { mode: PRIVATE_DIRECTORY_MODE });
    await mkdir(workDirectory, { mode: PRIVATE_DIRECTORY_MODE });
    await exportRepository(exactRepositoryRoot, snapshotRepository, release);
    await copyLockedRuntimeDependency(
      snapshotRepository,
      installedDependencyRoot,
    );
    assertDockerContextExclusions(
      await readFile(join(snapshotRepository, ".dockerignore"), "utf8"),
    );

    const snapshotControlPlaneRoot = join(
      snapshotRepository,
      "infra",
      "cloudflare",
      "control-plane",
    );
    const snapshotCloudflareRoot = dirname(snapshotControlPlaneRoot);
    const layout = Object.freeze({
      directory,
      repositoryRoot: snapshotRepository,
      controlPlaneRoot: snapshotControlPlaneRoot,
      cloudflareRoot: snapshotCloudflareRoot,
      workDirectory,
      reviewedEnvironmentFile: join(
        snapshotCloudflareRoot,
        "wrangler-production-empty.env",
      ),
      entrypointTarget: join(snapshotControlPlaneRoot, "src", "index.js"),
      source: release,
    });
    if (await readFile(layout.reviewedEnvironmentFile, "utf8") !==
        REVIEWED_ENV_FILE_CONTENT) {
      throw new Error("tagged release did not contain the reviewed empty Wrangler env file");
    }

    config = await createPrivateDeploymentConfig({
      prefix: PRIVATE_CONFIG_PREFIX,
      parentDirectory: snapshotCloudflareRoot,
      entrypointTarget: layout.entrypointTarget,
      render: (path) => render(path, layout),
      validate: validate == null
        ? undefined
        : (path) => validate(path, layout),
    });
    const configDirectoryRelative = relative(
      snapshotRepository,
      dirname(config.path),
    ).split(sep).join("/");
    if (!safeInventoryPath(configDirectoryRelative) ||
        !configDirectoryRelative.startsWith(
          "infra/cloudflare/witself-control-plane-deploy-",
        )) {
      throw new Error("private control-plane config escaped the release snapshot");
    }

    await freezeTree(snapshotRepository, configDirectoryRelative);
    const inventory = await scanTree(snapshotRepository, {
      maxFiles: MAX_SOURCE_FILES + MAX_DEPENDENCY_FILES,
      maxBytes: MAX_SOURCE_BYTES + MAX_DEPENDENCY_BYTES,
      maxFileBytes: MAX_SOURCE_FILE_BYTES,
      ignoredDirectory: configDirectoryRelative,
    });
    const configBytes = await config.readText();
    const evidence = Object.freeze({
      source_sha256: inventory.sha256,
      config_sha256: config.sha256,
      file_count: inventory.file_count,
      byte_count: inventory.byte_count,
      sha256: sha256(JSON.stringify({
        source_sha256: inventory.sha256,
        config_sha256: config.sha256,
      })),
    });

    const assertUnchanged = async () => {
      if (cleaned) {
        throw new Error("control-plane release snapshot was already cleaned up");
      }
      const [directoryMetadata, repositoryMetadata, workMetadata, current] =
        await Promise.all([
          lstat(directory),
          lstat(snapshotRepository),
          lstat(workDirectory),
          scanTree(snapshotRepository, {
            maxFiles: MAX_SOURCE_FILES + MAX_DEPENDENCY_FILES,
            maxBytes: MAX_SOURCE_BYTES + MAX_DEPENDENCY_BYTES,
            maxFileBytes: MAX_SOURCE_FILE_BYTES,
            ignoredDirectory: configDirectoryRelative,
          }),
          config.assertUnchanged(),
        ]);
      if (!directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
          (directoryMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
          !repositoryMetadata.isDirectory() || repositoryMetadata.isSymbolicLink() ||
          (repositoryMetadata.mode & 0o777) !== FROZEN_DIRECTORY_MODE ||
          !workMetadata.isDirectory() || workMetadata.isSymbolicLink() ||
          (workMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE ||
          !sameInventory(inventory, current)) {
        throw new Error("control-plane release snapshot changed during deployment");
      }
    };
    await assertUnchanged();

    return Object.freeze({
      ...layout,
      path: config.path,
      config,
      inventory: evidence,
      async readText() {
        await assertUnchanged();
        return configBytes;
      },
      assertUnchanged,
      async cleanup() {
        if (cleaned) return;
        const cleanupErrors = [];
        await thawDirectories(snapshotRepository).catch((error) => {
          cleanupErrors.push(error);
        });
        await config.cleanup().catch((error) => cleanupErrors.push(error));
        await rm(directory, { recursive: true, force: true })
          .catch((error) => cleanupErrors.push(error));
        cleaned = true;
        if (cleanupErrors.length === 1) throw cleanupErrors[0];
        if (cleanupErrors.length > 1) {
          throw new AggregateError(
            cleanupErrors,
            "control-plane release snapshot cleanup was incomplete",
          );
        }
      },
    });
  } catch (error) {
    const errors = [error];
    if (config != null) {
      await thawDirectories(snapshotRepository)
        .catch((cleanupError) => errors.push(cleanupError));
      await config.cleanup()
        .catch((cleanupError) => errors.push(cleanupError));
    }
    cleaned = true;
    await rm(directory, { recursive: true, force: true })
      .catch((cleanupError) => errors.push(cleanupError));
    if (errors.length === 1) throw error;
    throw new AggregateError(
      errors,
      "control-plane release snapshot creation failed and cleanup was incomplete",
    );
  }
}
