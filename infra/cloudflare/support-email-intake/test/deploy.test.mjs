import assert from "node:assert/strict";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { devNull, tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  assertReleaseUnchanged,
  productionDeploymentArguments,
  snapshotReleaseWorkerSource,
} from "../scripts/deploy.mjs";

const sourcePrefix = "infra/cloudflare/support-email-intake/src/";
const sourceFiles = [
  "authenticity.mjs",
  "index.js",
  "intake.mjs",
  "mime-text.mjs",
];
const commit = "a".repeat(40);

function taggedSourceInspection(files = sourceFiles) {
  const entries = files.map((path, index) => ({
    path,
    oid: String(index + 1).repeat(40),
    bytes: Buffer.from(`// tagged ${path}\n`, "utf8"),
  }));
  return (args) => {
    if (args[0] === "ls-tree") {
      return {
        status: 0,
        stdout: Buffer.from(entries.map((entry) =>
          `100644 blob ${entry.oid}\t${sourcePrefix}${entry.path}\0`).join("")),
      };
    }
    if (args[0] === "cat-file" && args[1] === "blob") {
      const entry = entries.find((candidate) => candidate.oid === args[2]);
      return entry
        ? { status: 0, stdout: entry.bytes }
        : { status: 1, stdout: Buffer.alloc(0) };
    }
    return { status: 1, stdout: Buffer.alloc(0) };
  };
}

test("deployment freezes exact tagged source bytes and detects mutation", async () => {
  const directory = await mkdtemp(join(tmpdir(), "support-email-source-"));
  try {
    await chmod(directory, 0o700);
    const snapshot = await snapshotReleaseWorkerSource(
      directory,
      commit,
      taggedSourceInspection(),
    );
    assert.equal(snapshot.file_count, 4);
    assert.match(snapshot.sha256, /^[0-9a-f]{64}$/);
    assert.equal(
      await readFile(snapshot.entrypoint, "utf8"),
      "// tagged index.js\n",
    );
    await snapshot.assertUnchanged();
    await chmod(snapshot.entrypoint, 0o600);
    await writeFile(snapshot.entrypoint, "// changed\n");
    await assert.rejects(
      () => snapshot.assertUnchanged(),
      /source snapshot changed during deployment/,
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});

test("deployment refuses an incomplete or expanded tagged source inventory", async () => {
  for (const files of [
    sourceFiles.slice(0, -1),
    [...sourceFiles, "unexpected.mjs"],
  ]) {
    const directory = await mkdtemp(join(tmpdir(), "support-email-inventory-"));
    try {
      await chmod(directory, 0o700);
      await assert.rejects(
        () => snapshotReleaseWorkerSource(
          directory,
          commit,
          taggedSourceInspection(files),
        ),
        /source inventory was invalid/,
      );
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  }
});

test("deployment re-verifies release identity and uses the frozen entrypoint", () => {
  const release = Object.freeze({
    version: "1.2.3",
    commit,
    date: "2026-08-24T00:00:00Z",
    tag: "v1.2.3",
    clean: true,
  });
  assert.equal(assertReleaseUnchanged(release, { ...release }), true);
  assert.throws(
    () => assertReleaseUnchanged(release, { ...release, commit: "b".repeat(40) }),
    /release source changed during deployment/,
  );
  const entrypoint = "/private/frozen/source/index.js";
  const config = "/private/frozen/wrangler.generated.jsonc";
  assert.deepEqual(productionDeploymentArguments(entrypoint, config, release), [
    "deploy", entrypoint, "--config", config,
    "--tag", "v1.2.3",
    "--message", `witself support email intake v1.2.3 (${commit})`,
    "--env-file", devNull,
  ]);
});

