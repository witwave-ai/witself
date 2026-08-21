import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import test from "node:test";

import {
  cleanupIsolatedWorkspace,
  compactIsolatedWorkspace,
  createIsolatedWorkspace,
  finalizeIsolatedWorkspace,
  readIsolatedArtifact,
} from "../lib/isolated-workspace.mjs";

const execFileAsync = promisify(execFile);
const gitBinary = process.platform === "win32" ? "git.exe" : "/usr/bin/git";

async function git(repository, args, options = {}) {
  const result = await execFileAsync(gitBinary, ["-C", repository, ...args], {
    encoding: options.encoding ?? "utf8",
    maxBuffer: 32 * 1024 * 1024,
    env: {
      PATH: process.env.PATH,
      HOME: options.home ?? path.dirname(repository),
      GIT_CONFIG_NOSYSTEM: "1",
      GIT_CONFIG_GLOBAL: process.platform === "win32" ? "NUL" : "/dev/null",
      GIT_TERMINAL_PROMPT: "0",
      ...options.env,
    },
  });
  return result.stdout;
}

async function fixture(t) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "witself-isolated-test-"));
  await fs.chmod(root, 0o700);
  const source = path.join(root, "source");
  const jobs = path.join(root, "jobs");
  await fs.mkdir(source, { mode: 0o700 });
  await fs.mkdir(jobs, { mode: 0o700 });
  await git(source, ["init", "--quiet"]);
  await git(source, ["config", "user.name", "Test User"]);
  await git(source, ["config", "user.email", "test@example.invalid"]);
  t.after(async () => { await fs.rm(root, { recursive: true, force: true }); });
  return { root, source: await fs.realpath(source), jobs: await fs.realpath(jobs) };
}

async function commitAll(repository, message = "base") {
  await git(repository, ["add", "--all"]);
  await git(repository, ["commit", "--quiet", "-m", message]);
  return (await git(repository, ["rev-parse", "HEAD"])).trim();
}

async function baseRepository(t) {
  const result = await fixture(t);
  await fs.writeFile(path.join(result.source, "clean.txt"), "clean base\n");
  await fs.writeFile(path.join(result.source, "staged.txt"), "staged base\n");
  await fs.writeFile(path.join(result.source, "unstaged.txt"), "unstaged base\n");
  await fs.mkdir(path.join(result.source, "links"));
  await fs.symlink("../clean.txt", path.join(result.source, "links", "clean-link"));
  result.head = await commitAll(result.source);
  return result;
}

async function artifactBytes(handle, descriptor) {
  const chunks = [];
  let offset = 0;
  do {
    const chunk = await readIsolatedArtifact(handle, { artifactId: descriptor.id, offset, maxBytes: Math.min(97, descriptor.maxChunkBytes) });
    chunks.push(Buffer.from(chunk.data, "base64"));
    offset = chunk.nextByteOffset;
    if (chunk.eof) break;
  } while (true);
  const result = Buffer.concat(chunks);
  assert.equal(result.length, descriptor.sizeBytes);
  return result;
}

async function rejectsCode(promise, code) {
  await assert.rejects(promise, (error) => error?.code === code);
}

test("dirty tracked and nonignored untracked content becomes an exact clean baseline; committed and uncommitted Codex deltas remain", async (t) => {
  const { source, jobs, head } = await baseRepository(t);
  await fs.writeFile(path.join(source, "staged.txt"), "user staged baseline\n");
  await git(source, ["add", "staged.txt"]);
  await fs.writeFile(path.join(source, "unstaged.txt"), "user unstaged baseline\n");
  await fs.writeFile(path.join(source, "untracked.txt"), "user untracked baseline\n");

  const sideTree = (await git(source, ["rev-parse", "HEAD^{tree}"])).trim();
  const sideCommit = (await git(source, ["commit-tree", sideTree, "-m", "side ref"], {
    env: {
      GIT_AUTHOR_NAME: "Side", GIT_AUTHOR_EMAIL: "side@example.invalid",
      GIT_COMMITTER_NAME: "Side", GIT_COMMITTER_EMAIL: "side@example.invalid",
    },
  })).trim();
  await git(source, ["update-ref", "refs/heads/private-side", sideCommit]);

  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  assert.equal(handle.originalHead, head);
  assert.equal(await fs.readFile(path.join(handle.workspaceRoot, "staged.txt"), "utf8"), "user staged baseline\n");
  assert.equal(await fs.readFile(path.join(handle.workspaceRoot, "unstaged.txt"), "utf8"), "user unstaged baseline\n");
  assert.equal(await fs.readFile(path.join(handle.workspaceRoot, "untracked.txt"), "utf8"), "user untracked baseline\n");
  assert.equal(await fs.readlink(path.join(handle.workspaceRoot, "links", "clean-link")), "../clean.txt");
  assert.equal(await git(handle.workspaceRoot, ["status", "--porcelain"]), "");
  assert.equal((await git(handle.workspaceRoot, ["rev-parse", `${handle.baselineCommit}^`])).trim(), head);
  assert.equal((await git(handle.workspaceRoot, ["cat-file", "-t", sideCommit])).trim(), "commit");
  assert.equal(await git(handle.workspaceRoot, ["remote"]), "");
  await assert.rejects(fs.lstat(path.join(handle.workspaceRoot, ".git", "objects", "info", "alternates")), { code: "ENOENT" });
  assert.deepEqual(await fs.readdir(path.join(path.dirname(handle.workspaceRoot), "empty-hooks")), []);

  await fs.writeFile(path.join(handle.workspaceRoot, "committed-delta.txt"), "committed by Codex\n");
  await git(handle.workspaceRoot, ["add", "committed-delta.txt"]);
  await git(handle.workspaceRoot, ["-c", "user.name=Codex", "-c", "user.email=codex@example.invalid", "commit", "--quiet", "-m", "Codex commit"]);
  await fs.writeFile(path.join(handle.workspaceRoot, "uncommitted-delta.txt"), "uncommitted by Codex\n");

  const result = await finalizeIsolatedWorkspace(handle, { evidence: { tests: "passed" } });
  assert.deepEqual(result.changedFiles, ["committed-delta.txt", "uncommitted-delta.txt"]);
  assert.equal(result.sourceDiverged, false);
  const patch = (await artifactBytes(handle, result.artifacts.patch)).toString("utf8");
  assert.match(patch, /committed-delta\.txt/);
  assert.match(patch, /uncommitted-delta\.txt/);
  assert.doesNotMatch(patch, /diff --git a\/staged\.txt/);
  assert.doesNotMatch(patch, /diff --git a\/unstaged\.txt/);
  assert.doesNotMatch(patch, /diff --git a\/untracked\.txt/);
  assert.deepEqual(JSON.parse((await artifactBytes(handle, result.artifacts.evidence)).toString("utf8")), { tests: "passed" });
  const workspace = handle.workspaceRoot;
  assert.deepEqual(await cleanupIsolatedWorkspace(handle), { cleaned: true, alreadyCleaned: false });
  await assert.rejects(fs.lstat(workspace), { code: "ENOENT" });
  assert.deepEqual(await cleanupIsolatedWorkspace(handle), { cleaned: true, alreadyCleaned: true });
});

test("a tracked deletion whose only parent directory vanished becomes a clean exact baseline", async (t) => {
  const { source, jobs } = await fixture(t);
  await fs.mkdir(path.join(source, "dir"));
  await fs.writeFile(path.join(source, "dir", "only.txt"), "tracked then deleted\n");
  await commitAll(source);
  await fs.rm(path.join(source, "dir", "only.txt"));
  await fs.rmdir(path.join(source, "dir"));
  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  await assert.rejects(fs.lstat(path.join(handle.workspaceRoot, "dir", "only.txt")), { code: "ENOENT" });
  assert.equal(await git(handle.workspaceRoot, ["status", "--porcelain"]), "");
  assert.equal(await git(handle.workspaceRoot, ["ls-tree", "-r", "--name-only", handle.baselineCommit]), "");
  await cleanupIsolatedWorkspace(handle);
});

test("capture copies loop partial writes and independently reject dishonest short storage", async (t) => {
  await t.test("reported partial progress is completed exactly", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const expected = await fs.readFile(path.join(source, "clean.txt"));
    const originalOpen = fs.open;
    let injected = false;
    fs.open = async function patchedOpen(value, ...args) {
      const handle = await originalOpen.call(this, value, ...args);
      if (String(value).includes(`${path.sep}source-capture${path.sep}`)) {
        const originalWrite = handle.write.bind(handle);
        handle.write = async (buffer, offset, length, position) => {
          if (!injected && length > 1) {
            injected = true;
            return originalWrite(buffer, offset, Math.floor(length / 2), position);
          }
          return originalWrite(buffer, offset, length, position);
        };
      }
      return handle;
    };
    let handle;
    try {
      handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
    } finally {
      fs.open = originalOpen;
    }
    assert.equal(injected, true);
    assert.deepEqual(await fs.readFile(path.join(handle.workspaceRoot, "clean.txt")), expected);
    assert.equal(await git(handle.workspaceRoot, ["status", "--porcelain"]), "");
    await cleanupIsolatedWorkspace(handle);
  });

  await t.test("a writer that lies about short progress fails independent size verification", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const originalOpen = fs.open;
    let injected = false;
    fs.open = async function patchedOpen(value, ...args) {
      const handle = await originalOpen.call(this, value, ...args);
      if (String(value).includes(`${path.sep}source-capture${path.sep}`)) {
        const originalWrite = handle.write.bind(handle);
        handle.write = async (buffer, offset, length, position) => {
          if (!injected && length > 1) {
            injected = true;
            const result = await originalWrite(buffer, offset, Math.floor(length / 2), position);
            return { ...result, bytesWritten: length };
          }
          return originalWrite(buffer, offset, length, position);
        };
      }
      return handle;
    };
    try {
      await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_snapshot_failed");
    } finally {
      fs.open = originalOpen;
    }
    assert.equal(injected, true);
    assert.deepEqual(await fs.readdir(jobs), []);
  });
});

test("missing-path reappearance races fail closed in source capture and result finalization", async (t) => {
  await t.test("source capture", async (t) => {
    const { source, jobs } = await fixture(t);
    const directory = path.join(source, "dir");
    const candidate = path.join(directory, "only.txt");
    await fs.mkdir(directory);
    await fs.writeFile(candidate, "tracked\n");
    await commitAll(source);
    await fs.rm(candidate);
    await fs.rmdir(directory);
    const originalLstat = fs.lstat;
    let candidateCalls = 0;
    fs.lstat = async function patchedLstat(value, ...args) {
      if (String(value) === candidate && ++candidateCalls === 2) {
        await fs.mkdir(directory);
        await fs.writeFile(candidate, "appeared during capture\n");
      }
      return originalLstat.call(this, value, ...args);
    };
    try {
      await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_drift");
    } finally {
      fs.lstat = originalLstat;
    }
    assert.ok(candidateCalls >= 2);
    assert.deepEqual(await fs.readdir(jobs), []);
  });

  await t.test("result finalization", async (t) => {
    const { source, jobs } = await fixture(t);
    await fs.mkdir(path.join(source, "dir"));
    await fs.writeFile(path.join(source, "dir", "only.txt"), "tracked\n");
    await commitAll(source);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
    const directory = path.join(handle.workspaceRoot, "dir");
    const candidate = path.join(directory, "only.txt");
    await fs.rm(candidate);
    await fs.rmdir(directory);
    const originalLstat = fs.lstat;
    let candidateCalls = 0;
    fs.lstat = async function patchedLstat(value, ...args) {
      if (String(value) === candidate && ++candidateCalls === 2) {
        await fs.mkdir(directory);
        await fs.writeFile(candidate, "appeared during finalize\n");
      }
      return originalLstat.call(this, value, ...args);
    };
    try {
      await rejectsCode(finalizeIsolatedWorkspace(handle), "isolated_workspace_drift");
    } finally {
      fs.lstat = originalLstat;
    }
    assert.ok(candidateCalls >= 2);
    await cleanupIsolatedWorkspace(handle);
  });
});

test("canonical ancestor checks detect an ABA symlink swap during result finalization", async (t) => {
  const { source, jobs } = await fixture(t);
  await fs.mkdir(path.join(source, "a", "b"), { recursive: true });
  await fs.writeFile(path.join(source, "a", "b", "deep.txt"), "base\n");
  await commitAll(source);
  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  await fs.writeFile(path.join(handle.workspaceRoot, "a", "b", "deep.txt"), "changed\n");
  const ancestor = path.join(handle.workspaceRoot, "a");
  const moved = path.join(handle.workspaceRoot, "a-moved");
  const checkedParent = path.join(handle.workspaceRoot, "a", "b");
  const originalRealpath = fs.realpath;
  let parentCalls = 0;
  let swapped = false;
  fs.realpath = async function patchedRealpath(value, ...args) {
    if (String(value) === checkedParent && ++parentCalls === 2) {
      await fs.rename(ancestor, moved);
      await fs.symlink("a-moved", ancestor);
      try {
        const resolved = await originalRealpath.call(this, value, ...args);
        swapped = true;
        return resolved;
      } finally {
        await fs.unlink(ancestor);
        await fs.rename(moved, ancestor);
      }
    }
    return originalRealpath.call(this, value, ...args);
  };
  try {
    await rejectsCode(finalizeIsolatedWorkspace(handle), "isolated_workspace_drift");
  } finally {
    fs.realpath = originalRealpath;
  }
  assert.equal(swapped, true);
  assert.equal(await fs.readFile(path.join(ancestor, "b", "deep.txt"), "utf8"), "changed\n");
  await cleanupIsolatedWorkspace(handle);
});

test("source drift after isolation is reported without discarding a safe patch", async (t) => {
  const { source, jobs } = await baseRepository(t);
  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  await fs.writeFile(path.join(handle.workspaceRoot, "clean.txt"), "Codex result\n");
  await fs.writeFile(path.join(source, "unstaged.txt"), "source moved after isolation\n");
  const result = await finalizeIsolatedWorkspace(handle, { evidence: "safe evidence\n" });
  assert.equal(result.sourceDiverged, true);
  assert.deepEqual(result.changedFiles, ["clean.txt"]);
  assert.ok((await artifactBytes(handle, result.artifacts.patch)).length > 0);
  await cleanupIsolatedWorkspace(handle);
});

test("source-root replacement is detected by inode identity and does not discard the isolated patch", async (t) => {
  const { root, source, jobs } = await baseRepository(t);
  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  await fs.writeFile(path.join(handle.workspaceRoot, "clean.txt"), "safe isolated result\n");
  const originalSource = path.join(root, "renamed-original-source");
  await fs.rename(source, originalSource);
  await fs.mkdir(source, { mode: 0o700 });
  await fs.writeFile(path.join(source, "replacement-marker"), "replacement must survive\n");
  const result = await finalizeIsolatedWorkspace(handle);
  assert.equal(result.sourceDiverged, true);
  assert.deepEqual(result.changedFiles, ["clean.txt"]);
  assert.ok((await artifactBytes(handle, result.artifacts.patch)).length > 0);
  assert.equal(await fs.readFile(path.join(source, "replacement-marker"), "utf8"), "replacement must survive\n");
  await cleanupIsolatedWorkspace(handle);
});

test("source and result symlinks cannot escape the isolated repository", async (t) => {
  await t.test("absolute source target", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.symlink("/etc/passwd", path.join(source, "absolute-link"));
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_unsafe_symlink");
  });
  await t.test("relative source escape", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.symlink("../outside", path.join(source, "escape-link"));
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_unsafe_symlink");
  });
  await t.test("Codex-created escape", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
    await fs.symlink("../../outside", path.join(handle.workspaceRoot, "escape-link"));
    await rejectsCode(finalizeIsolatedWorkspace(handle), "isolated_workspace_unsafe_symlink");
    await cleanupIsolatedWorkspace(handle);
  });
  await t.test("source links cannot pivot into Git metadata, including nested mixed-case forms", async (t) => {
    const direct = await baseRepository(t);
    await fs.symlink(".git/config", path.join(direct.source, "git-config-link"));
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: direct.source, jobsRoot: direct.jobs }), "isolated_workspace_unsafe_symlink");

    const nested = await baseRepository(t);
    await fs.mkdir(path.join(nested.source, "nested", "deeper"), { recursive: true });
    await fs.symlink("../../.GiT/hooks/pre-commit", path.join(nested.source, "nested", "deeper", "hook-link"));
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: nested.source, jobsRoot: nested.jobs }), "isolated_workspace_unsafe_symlink");
  });
  await t.test("result links cannot pivot into Git metadata but represented in-workspace links remain patchable", async (t) => {
    const hostile = await baseRepository(t);
    const hostileHandle = await createIsolatedWorkspace({ sourceRoot: hostile.source, jobsRoot: hostile.jobs });
    await fs.symlink(".git/config", path.join(hostileHandle.workspaceRoot, "git-config-link"));
    await rejectsCode(finalizeIsolatedWorkspace(hostileHandle), "isolated_workspace_unsafe_symlink");
    await cleanupIsolatedWorkspace(hostileHandle);

    const safe = await baseRepository(t);
    const safeHandle = await createIsolatedWorkspace({ sourceRoot: safe.source, jobsRoot: safe.jobs });
    await fs.symlink("clean.txt", path.join(safeHandle.workspaceRoot, "safe-link"));
    const result = await finalizeIsolatedWorkspace(safeHandle);
    assert.deepEqual(result.changedFiles, ["safe-link"]);
    assert.match((await artifactBytes(safeHandle, result.artifacts.patch)).toString("utf8"), /safe-link/);
    await cleanupIsolatedWorkspace(safeHandle);
  });
  await t.test("links to ignored or otherwise unrepresented paths are rejected", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.writeFile(path.join(source, ".gitignore"), "ignored-secret\n");
    await fs.writeFile(path.join(source, "ignored-secret"), "not captured\n");
    await fs.symlink("ignored-secret", path.join(source, "ignored-link"));
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_unsafe_symlink");
  });
});

test("source and isolated Git hooks, filters, fsmonitor, includes, and external diffs are inert", async (t) => {
  const { root, source, jobs } = await baseRepository(t);
  const attacks = {};
  for (const kind of ["hook", "fsmonitor", "filter", "diff"]) {
    const sentinel = path.join(root, `${kind}-ran`);
    const script = path.join(root, `${kind}.sh`);
    await fs.writeFile(script, `#!/bin/sh\n/usr/bin/touch ${JSON.stringify(sentinel)}\nexit 0\n`, { mode: 0o755 });
    attacks[kind] = { sentinel, script };
  }
  const assertInert = async () => {
    for (const [kind, { sentinel }] of Object.entries(attacks)) await assert.rejects(fs.lstat(sentinel), { code: "ENOENT" }, `${kind} command executed`);
  };
  const hostileHooks = path.join(root, "hostile-hooks");
  await fs.mkdir(hostileHooks);
  await fs.copyFile(attacks.hook.script, path.join(hostileHooks, "post-checkout"));
  await fs.chmod(path.join(hostileHooks, "post-checkout"), 0o755);
  await fs.writeFile(path.join(source, ".gitattributes"), "*.txt filter=evil diff=evil\n");
  await git(source, ["add", ".gitattributes"]);
  await git(source, ["config", "core.fsmonitor", attacks.fsmonitor.script]);
  await git(source, ["config", "core.hooksPath", hostileHooks]);
  await git(source, ["config", "filter.evil.clean", attacks.filter.script]);
  await git(source, ["config", "filter.evil.smudge", attacks.filter.script]);
  await git(source, ["config", "diff.evil.command", attacks.diff.script]);
  await assertInert();

  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  await assertInert();
  const isolatedConfig = path.join(handle.workspaceRoot, ".git", "config");
  await fs.appendFile(isolatedConfig, `\n[include]\n\tpath = ${JSON.stringify(path.join(source, ".git", "config"))}\n[diff]\n\texternal = ${JSON.stringify(attacks.diff.script)}\n[core]\n\thooksPath = ${JSON.stringify(hostileHooks)}\n`);
  await fs.writeFile(path.join(handle.workspaceRoot, "clean.txt"), "safe change\n");
  const result = await finalizeIsolatedWorkspace(handle);
  assert.deepEqual(result.changedFiles, ["clean.txt"]);
  await assertInert();
  await cleanupIsolatedWorkspace(handle);
});

test("operation deadlines, abort signals, and an early-closing Git stdin fail without wedging the lifecycle", async (t) => {
  await t.test("pre-aborted create and finalize operations release their lifecycle fence", async (t) => {
    const createFixture = await baseRepository(t);
    const createAbort = new AbortController();
    createAbort.abort();
    await rejectsCode(createIsolatedWorkspace({
      sourceRoot: createFixture.source,
      jobsRoot: createFixture.jobs,
      signal: createAbort.signal,
    }), "isolated_workspace_aborted");
    assert.deepEqual(await fs.readdir(createFixture.jobs), []);

    const finalizeFixture = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: finalizeFixture.source, jobsRoot: finalizeFixture.jobs });
    await fs.writeFile(path.join(handle.workspaceRoot, "clean.txt"), "result\n");
    const finalizeAbort = new AbortController();
    finalizeAbort.abort();
    await rejectsCode(finalizeIsolatedWorkspace(handle, { signal: finalizeAbort.signal }), "isolated_workspace_aborted");
    const result = await finalizeIsolatedWorkspace(handle);
    assert.deepEqual(result.changedFiles, ["clean.txt"]);
    await cleanupIsolatedWorkspace(handle);
  });

  await t.test("the end-to-end JavaScript deadline is checked before capture loops begin", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const originalNow = Date.now;
    const start = originalNow();
    let reads = 0;
    Date.now = () => start + (reads++ === 0 ? 0 : 100);
    try {
      await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs, deadlineMs: 10 }), "isolated_workspace_deadline");
    } finally {
      Date.now = originalNow;
    }
    assert.deepEqual(await fs.readdir(jobs), []);
  });

  await t.test("an early-closing Git child settles a large stdin write as a bounded failure", { skip: process.platform === "win32" }, async (t) => {
    const { root, source, jobs } = await baseRepository(t);
    for (let offset = 0; offset < 3_000; offset += 250) {
      await Promise.all(Array.from({ length: 250 }, (_, index) =>
        fs.writeFile(path.join(source, `object-${String(offset + index).padStart(4, "0")}.txt`), `object ${offset + index}\n`)));
    }
    await commitAll(source, "many reachable objects");
    const wrapper = path.join(root, "early-close-git.sh");
    await fs.writeFile(wrapper, `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    --batch-check=*) exec 0<&-; /bin/sleep 1; exit 17 ;;
  esac
done
exec /usr/bin/git "$@"
`, { mode: 0o755 });
    await assert.rejects(
      createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs, gitCommand: wrapper }),
      (error) => error?.code === "isolated_workspace_process_failed" || error?.code === "isolated_workspace_incomplete_history",
    );
    assert.deepEqual(await fs.readdir(jobs), []);
  });
});

test("incomplete histories and gitlinks fail closed", async (t) => {
  await t.test("gitlink", async (t) => {
    const { source, jobs, head } = await baseRepository(t);
    await git(source, ["update-index", "--add", "--cacheinfo", `160000,${head},vendor/submodule`]);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_submodule");
  });
  await t.test("gitlink retained only in reachable history", async (t) => {
    const { source, jobs, head } = await baseRepository(t);
    await git(source, ["update-index", "--add", "--cacheinfo", `160000,${head},vendor/historical-submodule`]);
    await git(source, ["commit", "--quiet", "-m", "historical gitlink"]);
    const gitlinkCommit = (await git(source, ["rev-parse", "HEAD"])).trim();
    await git(source, ["update-ref", "refs/heads/gitlink-history", gitlinkCommit]);
    await git(source, ["reset", "--quiet", "--hard", head]);
    assert.equal(await git(source, ["ls-files", "--stage", "vendor/historical-submodule"]), "");
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_submodule");
  });
  await t.test("merge-introduced gitlink hidden from raw history by a same-tree child and merge removal", async (t) => {
    const { source, jobs, head } = await baseRepository(t);
    const cleanTree = (await git(source, ["rev-parse", `${head}^{tree}`])).trim();
    const sameTreeParent = (await git(source, ["commit-tree", cleanTree, "-p", head, "-m", "same clean tree"])).trim();
    await git(source, ["update-index", "--add", "--cacheinfo", `160000,${head},vendor/merge-only-submodule`]);
    const gitlinkTree = (await git(source, ["write-tree"])).trim();
    await git(source, ["reset", "--quiet", head]);
    const introduced = (await git(source, ["commit-tree", gitlinkTree, "-p", head, "-p", sameTreeParent, "-m", "merge introduces gitlink"])).trim();
    const sameTreeChild = (await git(source, ["commit-tree", gitlinkTree, "-p", introduced, "-m", "same gitlink tree child"])).trim();
    const removed = (await git(source, ["commit-tree", cleanTree, "-p", sameTreeChild, "-p", sameTreeParent, "-m", "merge removes gitlink"])).trim();
    await git(source, ["update-ref", "refs/heads/merge-gitlink-history", removed]);
    assert.equal(await git(source, ["ls-files", "--stage", "vendor/merge-only-submodule"]), "");
    assert.doesNotMatch(await git(source, ["log", "--raw", "--format=", "--all", "HEAD"]), /160000/);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_submodule");
  });
  await t.test("promisor configuration", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await git(source, ["config", "remote.origin.promisor", "true"]);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_incomplete_history");
  });
  await t.test("canonical lowercase extension-only partial clone marker", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await git(source, ["config", "extensions.partialClone", "origin"]);
    assert.match(await git(source, ["config", "--local", "--name-only", "--get-regexp", "partialclone"]), /^extensions\.partialclone/m);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_incomplete_history");
  });
  await t.test("missing reachable object", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const oid = (await git(source, ["rev-parse", "HEAD:clean.txt"])).trim();
    await fs.rm(path.join(source, ".git", "objects", oid.slice(0, 2), oid.slice(2)));
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_incomplete_history");
  });
  await t.test("shallow clone", async (t) => {
    const { root, source } = await baseRepository(t);
    await fs.writeFile(path.join(source, "second.txt"), "second\n");
    await commitAll(source, "second");
    const shallow = path.join(root, "shallow");
    const jobs = path.join(root, "shallow-jobs");
    await fs.mkdir(jobs, { mode: 0o700 });
    await execFileAsync(gitBinary, ["clone", "--quiet", "--depth=1", `file://${source}`, shallow], { env: { ...process.env, GIT_TERMINAL_PROMPT: "0" } });
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: shallow, jobsRoot: jobs }), "isolated_workspace_incomplete_history");
  });
});

test("sparse-checkout configuration and skip-worktree entries fail closed before snapshotting", async (t) => {
  await t.test("standalone skip-worktree flag", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await git(source, ["update-index", "--skip-worktree", "clean.txt"]);
    assert.equal(await fs.readFile(path.join(source, "clean.txt"), "utf8"), "clean base\n");
    assert.match(await git(source, ["ls-files", "-v", "clean.txt"]), /^[Ss] /);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_sparse_checkout");
  });

  await t.test("skipped child under an existing sparse parent", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const parent = path.join(source, "parent");
    await fs.mkdir(parent);
    await fs.writeFile(path.join(parent, "kept.txt"), "kept\n");
    await fs.writeFile(path.join(parent, "skipped.txt"), "skipped tracked content\n");
    await commitAll(source, "add sparse parent files");
    await git(source, ["sparse-checkout", "init", "--no-cone"]);
    await git(source, ["sparse-checkout", "set", "--no-cone", "parent/kept.txt"]);
    assert.equal(await fs.readFile(path.join(parent, "kept.txt"), "utf8"), "kept\n");
    await assert.rejects(fs.lstat(path.join(parent, "skipped.txt")), { code: "ENOENT" });
    assert.match(await git(source, ["ls-files", "-v", "parent/skipped.txt"]), /^[Ss] /);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_sparse_checkout");
  });

  await t.test("stale disabled sparse configuration is rejected rather than inferred", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await git(source, ["config", "core.sparseCheckout", "false"]);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_sparse_checkout");
  });
});

test("reachable history, bundle, and clone disk envelopes are finite and enforced before use", async (t) => {
  await t.test("a deleted oversized historical blob is rejected before bundle creation", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.writeFile(path.join(source, "deleted-large.bin"), Buffer.alloc(4_096, 0x61));
    await commitAll(source, "add historical large blob");
    await fs.rm(path.join(source, "deleted-large.bin"));
    await commitAll(source, "remove historical large blob");
    await rejectsCode(createIsolatedWorkspace({
      sourceRoot: source,
      jobsRoot: jobs,
      limits: { maxHistoryObjectBytes: 1_024 },
    }), "isolated_workspace_history_limit");
    assert.deepEqual(await fs.readdir(jobs), []);
  });

  await t.test("unique reachable object count and cumulative bytes are bounded", async (t) => {
    const first = await baseRepository(t);
    await rejectsCode(createIsolatedWorkspace({
      sourceRoot: first.source,
      jobsRoot: first.jobs,
      limits: { maxHistoryObjects: 2 },
    }), "isolated_workspace_history_limit");
    assert.deepEqual(await fs.readdir(first.jobs), []);

    const second = await baseRepository(t);
    await rejectsCode(createIsolatedWorkspace({
      sourceRoot: second.source,
      jobsRoot: second.jobs,
      limits: { maxHistoryBytes: 64 },
    }), "isolated_workspace_history_limit");
    assert.deepEqual(await fs.readdir(second.jobs), []);
  });

  await t.test("written bundle and clone are checked against their independent envelopes", async (t) => {
    const bundled = await baseRepository(t);
    await rejectsCode(createIsolatedWorkspace({
      sourceRoot: bundled.source,
      jobsRoot: bundled.jobs,
      limits: { maxBundleBytes: 1 },
    }), "isolated_workspace_history_limit");
    assert.deepEqual(await fs.readdir(bundled.jobs), []);

    const cloned = await baseRepository(t);
    await rejectsCode(createIsolatedWorkspace({
      sourceRoot: cloned.source,
      jobsRoot: cloned.jobs,
      limits: { maxCloneBytes: 1 },
    }), "isolated_workspace_disk_policy");
    assert.deepEqual(await fs.readdir(cloned.jobs), []);
  });
});

test("free-space and early-create failures occur before capture and leave no UUID job residue", async (t) => {
  await t.test("low free space is rejected before source-capture creation", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const originalStatfs = fs.statfs;
    const originalMkdir = fs.mkdir;
    let captureCreated = false;
    fs.statfs = async () => ({ bavail: 1n, bsize: 1n });
    fs.mkdir = async function patchedMkdir(value, ...args) {
      if (path.basename(String(value)) === "source-capture") captureCreated = true;
      return originalMkdir.call(this, value, ...args);
    };
    try {
      await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_disk_policy");
    } finally {
      fs.statfs = originalStatfs;
      fs.mkdir = originalMkdir;
    }
    assert.equal(captureCreated, false);
    assert.deepEqual(await fs.readdir(jobs), []);
  });

  await t.test("failure while creating private children removes the exact pre-marker job inode", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const originalMkdir = fs.mkdir;
    let injected = false;
    fs.mkdir = async function patchedMkdir(value, ...args) {
      const candidate = String(value);
      if (!injected && path.basename(candidate) === "home" && path.dirname(path.dirname(candidate)) === jobs) {
        injected = true;
        const error = new Error("injected early create failure");
        error.code = "ENOSPC";
        throw error;
      }
      return originalMkdir.call(this, value, ...args);
    };
    try {
      await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_failed");
    } finally {
      fs.mkdir = originalMkdir;
    }
    assert.equal(injected, true);
    assert.deepEqual(await fs.readdir(jobs), []);
  });
});

test("special files, unsafe paths, special modes, and byte limits fail closed", async (t) => {
  await t.test("FIFO", { skip: process.platform === "win32" }, async (t) => {
    const { source, jobs } = await baseRepository(t);
    await execFileAsync("/usr/bin/mkfifo", [path.join(source, "named-pipe")]);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_special_file");
  });
  await t.test("newline path", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.writeFile(path.join(source, "bad\npath"), "bad\n");
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_unsafe_path");
  });
  await t.test("set-id mode", { skip: process.platform === "win32" }, async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.chmod(path.join(source, "clean.txt"), 0o4755);
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs }), "isolated_workspace_drift");
  });
  await t.test("file byte ceiling", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.writeFile(path.join(source, "clean.txt"), "12345");
    await rejectsCode(createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs, limits: { maxFileBytes: 4 } }), "isolated_workspace_limit");
  });
});

test("changed-file, patch, evidence, and artifact read bounds are enforced", async (t) => {
  await t.test("changed file count", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs, limits: { maxChangedFiles: 1 } });
    await fs.writeFile(path.join(handle.workspaceRoot, "one.txt"), "one\n");
    await fs.writeFile(path.join(handle.workspaceRoot, "two.txt"), "two\n");
    await assert.rejects(finalizeIsolatedWorkspace(handle), (error) => error?.code === "isolated_workspace_limit" || error?.code === "isolated_workspace_change_limit");
    await cleanupIsolatedWorkspace(handle);
  });
  await t.test("patch bytes", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs, limits: { maxPatchBytes: 128 } });
    await fs.writeFile(path.join(handle.workspaceRoot, "large-change.txt"), "x".repeat(1_024));
    await rejectsCode(finalizeIsolatedWorkspace(handle), "isolated_workspace_patch_limit");
    await cleanupIsolatedWorkspace(handle);
  });
  await t.test("evidence and reads", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs, limits: { maxEvidenceBytes: 8, maxArtifactChunkBytes: 4 } });
    for (let attempt = 0; attempt < 3; attempt += 1) {
      await rejectsCode(finalizeIsolatedWorkspace(handle, { evidence: "123456789" }), "isolated_workspace_evidence_limit");
      assert.deepEqual((await fs.readdir(path.dirname(handle.workspaceRoot))).filter((name) => name.startsWith("result-capture-")), []);
    }
    const result = await finalizeIsolatedWorkspace(handle, { evidence: "12345678" });
    assert.deepEqual((await fs.readdir(path.dirname(handle.workspaceRoot))).filter((name) => name.startsWith("result-capture-")), []);
    await rejectsCode(readIsolatedArtifact(handle, { artifactId: result.artifacts.evidence.id, offset: 0, maxBytes: 5 }), "isolated_workspace_artifact_range");
    const first = await readIsolatedArtifact(handle, { artifactId: result.artifacts.evidence.id, offset: 0, maxBytes: 4 });
    assert.equal(Buffer.from(first.data, "base64").toString(), "1234");
    await cleanupIsolatedWorkspace(handle);
  });
  await t.test("ignored result files count toward the all-regular-file workspace envelope", async (t) => {
    const { source, jobs } = await baseRepository(t);
    await fs.writeFile(path.join(source, ".gitignore"), "ignored-result.bin\n");
    await commitAll(source, "ignore generated result");
    const handle = await createIsolatedWorkspace({
      sourceRoot: source,
      jobsRoot: jobs,
      limits: { maxFileBytes: 1_024, maxTotalBytes: 1_024 },
    });
    await fs.writeFile(path.join(handle.workspaceRoot, "ignored-result.bin"), Buffer.alloc(2_048, 0x49));
    await rejectsCode(finalizeIsolatedWorkspace(handle), "isolated_workspace_limit");
    await cleanupIsolatedWorkspace(handle);
  });
});

test("git diff --check rejects whitespace errors", async (t) => {
  const { source, jobs } = await baseRepository(t);
  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  await fs.writeFile(path.join(handle.workspaceRoot, "whitespace.txt"), "trailing space \n");
  await rejectsCode(finalizeIsolatedWorkspace(handle), "isolated_workspace_diff_check");
  await cleanupIsolatedWorkspace(handle);
});

test("finalized jobs compact to immutable artifacts and remain readable across sequential large workspaces", async (t) => {
  const { source, jobs } = await baseRepository(t);
  await fs.writeFile(path.join(source, "large-baseline.bin"), Buffer.alloc(1024 * 1024, 0x5a));
  await commitAll(source, "large baseline");
  const handles = [];
  for (let index = 0; index < 2; index += 1) {
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
    await fs.writeFile(path.join(handle.workspaceRoot, `result-${index}.txt`), `result ${index}\n`);
    const result = await finalizeIsolatedWorkspace(handle, { evidence: { index } });
    const expectedPatch = await artifactBytes(handle, result.artifacts.patch);
    const expectedEvidence = await artifactBytes(handle, result.artifacts.evidence);
    const jobRoot = path.dirname(handle.workspaceRoot);
    const compacted = await compactIsolatedWorkspace(handle);
    assert.equal(compacted.compacted, true);
    assert.equal(compacted.alreadyCompacted, false);
    const artifactBytesRetained = result.artifacts.patch.sizeBytes + result.artifacts.evidence.sizeBytes;
    assert.ok(compacted.retainedBytes >= artifactBytesRetained && compacted.retainedBytes <= artifactBytesRetained + 256);
    assert.deepEqual((await fs.readdir(jobRoot)).sort(), [".witself-owner", "artifacts"]);
    assert.deepEqual(await fs.readdir(path.join(jobRoot, "artifacts")), ["published"]);
    await assert.rejects(fs.lstat(handle.workspaceRoot), { code: "ENOENT" });
    await assert.rejects(fs.lstat(path.join(jobRoot, "source-capture")), { code: "ENOENT" });
    assert.deepEqual(await artifactBytes(handle, result.artifacts.patch), expectedPatch);
    assert.deepEqual(await artifactBytes(handle, result.artifacts.evidence), expectedEvidence);
    assert.deepEqual(await compactIsolatedWorkspace(handle), {
      compacted: true,
      alreadyCompacted: true,
      retainedBytes: compacted.retainedBytes,
    });
    assert.strictEqual(await finalizeIsolatedWorkspace(handle), result);
    handles.push({ handle, jobRoot });
  }
  assert.equal((await fs.readdir(jobs)).length, 2);
  for (const { handle, jobRoot } of handles) {
    await cleanupIsolatedWorkspace(handle);
    await assert.rejects(fs.lstat(jobRoot), { code: "ENOENT" });
  }
  assert.deepEqual(await fs.readdir(jobs), []);
});

test("compaction fails closed on foreign payload and safely retries a quarantined partial removal", async (t) => {
  await t.test("foreign root entry", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
    const result = await finalizeIsolatedWorkspace(handle);
    const jobRoot = path.dirname(handle.workspaceRoot);
    await fs.writeFile(path.join(jobRoot, "foreign-entry"), "must not be silently compacted\n");
    await rejectsCode(compactIsolatedWorkspace(handle), "isolated_workspace_compaction_failed");
    assert.ok((await artifactBytes(handle, result.artifacts.patch)).length >= 0);
    await fs.rm(path.join(jobRoot, "foreign-entry"));
    assert.equal((await compactIsolatedWorkspace(handle)).compacted, true);
    await cleanupIsolatedWorkspace(handle);
  });

  await t.test("partial quarantine removal", async (t) => {
    const { source, jobs } = await baseRepository(t);
    const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
    const result = await finalizeIsolatedWorkspace(handle, { evidence: "retained\n" });
    const originalRm = fs.rm;
    let injected = false;
    fs.rm = async function patchedRm(value, ...args) {
      if (!injected && path.basename(String(value)).startsWith(".compact-")) {
        injected = true;
        const error = new Error("injected compaction removal failure");
        error.code = "EBUSY";
        throw error;
      }
      return originalRm.call(this, value, ...args);
    };
    try {
      await rejectsCode(compactIsolatedWorkspace(handle), "isolated_workspace_compaction_failed");
    } finally {
      fs.rm = originalRm;
    }
    assert.equal(injected, true);
    const compacted = await compactIsolatedWorkspace(handle);
    assert.equal(compacted.compacted, true);
    assert.equal((await artifactBytes(handle, result.artifacts.evidence)).toString("utf8"), "retained\n");
    await cleanupIsolatedWorkspace(handle);
  });
});

test("cleanup refuses a renamed-and-replaced job path and deletes only the restored owned inode", async (t) => {
  const { source, jobs } = await baseRepository(t);
  const handle = await createIsolatedWorkspace({ sourceRoot: source, jobsRoot: jobs });
  const jobRoot = path.dirname(handle.workspaceRoot);
  const moved = path.join(jobs, "moved-owned-job");
  await fs.rename(jobRoot, moved);
  await fs.mkdir(jobRoot, { mode: 0o700 });
  await fs.writeFile(path.join(jobRoot, ".witself-owner"), "forged\n");
  await fs.writeFile(path.join(jobRoot, "must-survive"), "safe\n");
  await rejectsCode(cleanupIsolatedWorkspace(handle), "isolated_workspace_owner_mismatch");
  assert.equal(await fs.readFile(path.join(jobRoot, "must-survive"), "utf8"), "safe\n");
  await fs.rm(jobRoot, { recursive: true });
  await fs.rename(moved, jobRoot);
  await cleanupIsolatedWorkspace(handle);
  await assert.rejects(fs.lstat(jobRoot), { code: "ENOENT" });
});
