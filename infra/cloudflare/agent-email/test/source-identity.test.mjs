import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  assertReleaseSource,
  sourceIdentity,
} from "../scripts/source-identity.mjs";

function git(root, ...args) {
  return execFileSync("git", args, { cwd: root, encoding: "utf8" }).trim();
}

async function repository() {
  const root = await mkdtemp(join(tmpdir(), "witself-edge-identity-"));
  git(root, "init", "--quiet");
  git(root, "config", "user.name", "Witself test");
  git(root, "config", "user.email", "test@witself.invalid");
  await writeFile(join(root, "tracked.txt"), "identity\n");
  git(root, "add", "tracked.txt");
  execFileSync("git", ["commit", "--quiet", "-m", "identity fixture"], {
    cwd: root,
    env: {
      ...process.env,
      GIT_AUTHOR_DATE: "2026-08-09T12:00:00Z",
      GIT_COMMITTER_DATE: "2026-08-09T12:00:00Z",
    },
  });
  return root;
}

test("source identity is immutable but non-release on an untagged checkout", async () => {
  const root = await repository();
  const identity = sourceIdentity({ repositoryRoot: root });
  assert.match(identity.version, /^development-[0-9a-f]{12}$/);
  assert.match(identity.commit, /^[0-9a-f]{40}$/);
  assert.match(identity.date, /^2026-08-09T12:00:00(?:Z|\+00:00)$/);
  assert.equal(identity.tag, "");
  assert.equal(identity.clean, true);
  assert.throws(
    () => sourceIdentity({ repositoryRoot: root, requireRelease: true }),
    /clean checkout at one exact semantic-version tag/,
  );
});

test("release source requires exactly one semantic tag and a clean tree", async () => {
  const root = await repository();
  git(root, "tag", "v1.2.3");
  const release = sourceIdentity({ repositoryRoot: root, requireRelease: true });
  assert.equal(release.version, "1.2.3");
  assert.equal(release.tag, "v1.2.3");
  assert.equal(assertReleaseSource(release), release);

  await writeFile(join(root, "tracked.txt"), "dirty\n");
  const dirty = sourceIdentity({ repositoryRoot: root });
  assert.match(dirty.version, /-dirty$/);
  assert.equal(dirty.clean, false);
  assert.throws(
    () => sourceIdentity({ repositoryRoot: root, requireRelease: true }),
    /clean checkout at one exact semantic-version tag/,
  );
});

test("release source rejects multiple semantic tags at the same commit", async () => {
  const root = await repository();
  git(root, "tag", "v1.2.3");
  git(root, "tag", "v1.2.4");
  const identity = sourceIdentity({ repositoryRoot: root });
  assert.equal(identity.tag, "");
  assert.match(identity.version, /^development-/);
  assert.throws(
    () => sourceIdentity({ repositoryRoot: root, requireRelease: true }),
    /clean checkout at one exact semantic-version tag/,
  );
});
