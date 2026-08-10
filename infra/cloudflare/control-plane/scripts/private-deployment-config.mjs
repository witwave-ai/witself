import { createHash } from "node:crypto";
import {
  chmod,
  lstat,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import {
  isAbsolute,
  join,
  relative,
  resolve,
  sep,
} from "node:path";

const MAX_CONFIG_BYTES = 5 * 1024 * 1024;
const PRIVATE_DIRECTORY_MODE = 0o700;
const IMMUTABLE_FILE_MODE = 0o400;
const WRANGLER_MAIN = '"main": "src/index.js"';

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function readExactPrivateConfig(path, expectedSHA256 = "", directory = "") {
  const [metadata, directoryMetadata] = await Promise.all([
    lstat(path),
    directory === "" ? null : lstat(directory),
  ]);
  if ((directoryMetadata != null &&
        (!directoryMetadata.isDirectory() || directoryMetadata.isSymbolicLink() ||
         (directoryMetadata.mode & 0o777) !== PRIVATE_DIRECTORY_MODE)) ||
      !metadata.isFile() || metadata.isSymbolicLink() ||
      (metadata.mode & 0o777) !== IMMUTABLE_FILE_MODE ||
      metadata.size < 1 || metadata.size > MAX_CONFIG_BYTES) {
    throw new Error("private deployment configuration had unsafe metadata");
  }
  const bytes = await readFile(path);
  if (bytes.byteLength !== metadata.size ||
      (expectedSHA256 !== "" && sha256(bytes) !== expectedSHA256)) {
    throw new Error("private deployment configuration changed during deployment");
  }
  return bytes;
}

async function validateRelocation(parentDirectory, entrypointTarget) {
  if ((parentDirectory == null) !== (entrypointTarget == null)) {
    throw new Error(
      "private deployment configuration relocation requires both parentDirectory and entrypointTarget",
    );
  }
  if (parentDirectory == null) return null;
  if (typeof parentDirectory !== "string" ||
      typeof entrypointTarget !== "string" ||
      !isAbsolute(parentDirectory) || !isAbsolute(entrypointTarget) ||
      resolve(parentDirectory) !== parentDirectory ||
      resolve(entrypointTarget) !== entrypointTarget) {
    throw new Error(
      "private deployment configuration relocation paths must be normalized absolute paths",
    );
  }
  const [parentMetadata, entrypointMetadata] = await Promise.all([
    lstat(parentDirectory),
    lstat(entrypointTarget),
  ]);
  if (!parentMetadata.isDirectory() || parentMetadata.isSymbolicLink() ||
      !entrypointMetadata.isFile() || entrypointMetadata.isSymbolicLink()) {
    throw new Error(
      "private deployment configuration relocation paths had unsafe metadata",
    );
  }
  const relativeTarget = relative(parentDirectory, entrypointTarget);
  if (relativeTarget === "" || isAbsolute(relativeTarget) ||
      relativeTarget === ".." || relativeTarget.startsWith(`..${sep}`)) {
    throw new Error(
      "private deployment configuration entrypoint must be inside its parent directory",
    );
  }
  return Object.freeze({ parentDirectory, entrypointTarget });
}

async function relocateWranglerEntrypoint(path, directory, entrypointTarget) {
  const metadata = await lstat(path);
  if (!metadata.isFile() || metadata.isSymbolicLink() ||
      metadata.size < 1 || metadata.size > MAX_CONFIG_BYTES) {
    throw new Error("rendered private deployment configuration had unsafe metadata");
  }
  const text = await readFile(path, "utf8");
  const occurrences = text.split(WRANGLER_MAIN).length - 1;
  if (occurrences !== 1) {
    throw new Error(
      "rendered private deployment configuration must contain the exact Wrangler main entrypoint once",
    );
  }
  const relocatedEntrypoint = relative(directory, entrypointTarget)
    .split(sep)
    .join("/");
  if (!/^(?:\.\.\/)*[A-Za-z0-9._-]+(?:\/[A-Za-z0-9._-]+)*$/.test(
    relocatedEntrypoint,
  ) || resolve(directory, relocatedEntrypoint) !== entrypointTarget) {
    throw new Error(
      "private deployment configuration entrypoint relocation was unsafe",
    );
  }
  const relocated = text.replace(
    WRANGLER_MAIN,
    `"main": ${JSON.stringify(relocatedEntrypoint)}`,
  );
  if (Buffer.byteLength(relocated) > MAX_CONFIG_BYTES) {
    throw new Error("relocated private deployment configuration was too large");
  }
  await writeFile(path, relocated, { mode: 0o600 });
}

// A generated Wrangler file is an input to a provider mutation, not a shared
// build artifact. Give each invocation an unpredictable private directory and
// freeze the file read-only before it can enter the global operations lease.
// Concurrent supported invocations can then render different cohorts or gates
// without either process ever opening the other's bytes.
export async function createPrivateDeploymentConfig({
  prefix,
  render,
  validate,
  parentDirectory,
  entrypointTarget,
}) {
  if (!/^[a-z0-9][a-z0-9-]{0,62}-$/.test(prefix) ||
      typeof render !== "function" ||
      (validate != null && typeof validate !== "function")) {
    throw new Error("private deployment configuration options were invalid");
  }
  const relocation = await validateRelocation(
    parentDirectory,
    entrypointTarget,
  );
  const directory = await mkdtemp(join(
    relocation?.parentDirectory ?? tmpdir(),
    prefix,
  ));
  const path = join(directory, "wrangler.generated.jsonc");
  let cleaned = false;
  try {
    await chmod(directory, PRIVATE_DIRECTORY_MODE);
    await render(path);
    if (relocation) {
      await relocateWranglerEntrypoint(
        path,
        directory,
        relocation.entrypointTarget,
      );
    }
    if (validate) await validate(path);
    await chmod(path, IMMUTABLE_FILE_MODE);
    const bytes = await readExactPrivateConfig(path, "", directory);
    const digest = sha256(bytes);
    return Object.freeze({
      path,
      sha256: digest,
      async assertUnchanged() {
        if (cleaned) {
          throw new Error("private deployment configuration was already cleaned up");
        }
        await readExactPrivateConfig(path, digest, directory);
      },
      async readText() {
        if (cleaned) {
          throw new Error("private deployment configuration was already cleaned up");
        }
        return (await readExactPrivateConfig(path, digest, directory))
          .toString("utf8");
      },
      async cleanup() {
        if (cleaned) return;
        await rm(directory, { recursive: true, force: true });
        cleaned = true;
      },
    });
  } catch (error) {
    cleaned = true;
    await rm(directory, { recursive: true, force: true }).catch(() => {});
    throw error;
  }
}
