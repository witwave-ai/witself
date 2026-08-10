import { createHash } from "node:crypto";
import {
  chmod,
  lstat,
  mkdtemp,
  readFile,
  rm,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const MAX_CONFIG_BYTES = 5 * 1024 * 1024;
const PRIVATE_DIRECTORY_MODE = 0o700;
const IMMUTABLE_FILE_MODE = 0o400;

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function readExactPrivateConfig(path, expectedSHA256 = "") {
  const metadata = await lstat(path);
  if (!metadata.isFile() || metadata.isSymbolicLink() ||
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

// A generated Wrangler file is an input to a provider mutation, not a shared
// build artifact. Give each invocation an unpredictable private directory and
// freeze the file read-only before it can enter the global operations lease.
// Concurrent supported invocations can then render different cohorts or gates
// without either process ever opening the other's bytes.
export async function createPrivateDeploymentConfig({
  prefix,
  render,
  validate,
}) {
  if (!/^[a-z0-9][a-z0-9-]{0,62}-$/.test(prefix) ||
      typeof render !== "function" ||
      (validate != null && typeof validate !== "function")) {
    throw new Error("private deployment configuration options were invalid");
  }
  const directory = await mkdtemp(join(tmpdir(), prefix));
  await chmod(directory, PRIVATE_DIRECTORY_MODE);
  const path = join(directory, "wrangler.generated.jsonc");
  let cleaned = false;
  try {
    await render(path);
    if (validate) await validate(path);
    await chmod(path, IMMUTABLE_FILE_MODE);
    const bytes = await readExactPrivateConfig(path);
    const digest = sha256(bytes);
    return Object.freeze({
      path,
      sha256: digest,
      async assertUnchanged() {
        if (cleaned) {
          throw new Error("private deployment configuration was already cleaned up");
        }
        await readExactPrivateConfig(path, digest);
      },
      async readText() {
        if (cleaned) {
          throw new Error("private deployment configuration was already cleaned up");
        }
        return (await readExactPrivateConfig(path, digest)).toString("utf8");
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
