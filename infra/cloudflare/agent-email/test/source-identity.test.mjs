import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
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

test("source identity ignores Git environment repository redirection", async () => {
  const trusted = await repository();
  const redirected = await repository();
  try {
    git(trusted, "tag", "v1.2.3");
    git(redirected, "tag", "v9.9.9");
    const trustedCommit = git(trusted, "rev-parse", "HEAD");
    const identity = sourceIdentity({
      repositoryRoot: trusted,
      requireRelease: true,
      environment: {
        ...process.env,
        GIT_DIR: join(redirected, ".git"),
        GIT_WORK_TREE: redirected,
        GIT_OBJECT_DIRECTORY: join(redirected, ".git", "objects"),
        GIT_CONFIG_COUNT: "1",
        GIT_CONFIG_KEY_0: "core.bare",
        GIT_CONFIG_VALUE_0: "true",
      },
    });
    assert.equal(identity.commit, trustedCommit);
    assert.equal(identity.version, "1.2.3");
    assert.equal(identity.tag, "v1.2.3");
  } finally {
    await Promise.all([
      rm(trusted, { recursive: true, force: true }),
      rm(redirected, { recursive: true, force: true }),
    ]);
  }
});

test("source identity ignores repository-local commit replacement refs", async () => {
  const root = await repository();
  try {
    git(root, "tag", "v1.2.3");
    const originalCommit = git(root, "rev-parse", "HEAD");
    const tree = git(root, "rev-parse", "HEAD^{tree}");
    const replacementCommit = execFileSync(
      "git",
      ["commit-tree", tree, "-m", "replacement commit"],
      {
        cwd: root,
        encoding: "utf8",
        env: {
          ...process.env,
          GIT_AUTHOR_DATE: "2030-01-02T03:04:05Z",
          GIT_COMMITTER_DATE: "2030-01-02T03:04:05Z",
        },
      },
    ).trim();
    git(root, "replace", originalCommit, replacementCommit);
    assert.match(
      git(root, "show", "-s", "--format=%cI", "HEAD"),
      /^2030-01-02T03:04:05(?:Z|\+00:00)$/,
    );

    const identity = sourceIdentity({
      repositoryRoot: root,
      requireRelease: true,
      environment: {
        ...process.env,
        GIT_NO_REPLACE_OBJECTS: "0",
      },
    });
    assert.equal(identity.commit, originalCommit);
    assert.match(identity.date, /^2026-08-09T12:00:00(?:Z|\+00:00)$/);
    assert.equal(identity.tag, "v1.2.3");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
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
