import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import { assertWranglerVersion, workerDirectories } from "../../wrangler-version.mjs";

async function writeJSON(root, path, value) {
  const destination = join(root, path);
  await mkdir(dirname(destination), { recursive: true });
  await writeFile(destination, JSON.stringify(value));
}

async function fixture(t, version = "4.200.0") {
  const root = await mkdtemp(join(tmpdir(), "witself-wrangler-version-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  await writeJSON(root, "wrangler-version.json", { version });
  for (const worker of workerDirectories) {
    await writeJSON(root, `${worker}/package.json`, {
      devDependencies: { wrangler: version },
    });
  }
  return root;
}

test("a coherent major bump uses the shared release without another literal test pin", async (t) => {
  const root = await fixture(t, "5.0.0");
  for (const worker of workerDirectories) {
    await writeJSON(root, `${worker}/node_modules/wrangler/package.json`, { version: "5.0.0" });
    await assertWranglerVersion(worker, root);
  }
});

test("each Worker gate rejects an isolated bump in any other Worker", async (t) => {
  const root = await fixture(t);
  for (const bumpedWorker of workerDirectories) {
    await writeJSON(root, `${bumpedWorker}/package.json`, {
      devDependencies: { wrangler: "4.201.0" },
    });
    for (const worker of workerDirectories) {
      await assert.rejects(assertWranglerVersion(worker, root), (error) => {
        assert.equal(error.code, "ERR_ASSERTION");
        assert.match(error.message, new RegExp(`${bumpedWorker}/package\\.json`));
        assert.match(error.message, /infra\/cloudflare\/wrangler-version\.json/);
        assert.match(error.message, /all four Worker directories/);
        return true;
      });
    }
    await writeJSON(root, `${bumpedWorker}/package.json`, {
      devDependencies: { wrangler: "4.200.0" },
    });
  }
});

test("a matching range is rejected because every Worker must pin an exact release", async (t) => {
  const root = await fixture(t);
  await writeJSON(root, "agent-email/package.json", {
    devDependencies: { wrangler: "^4.200.0" },
  });
  await assert.rejects(
    assertWranglerVersion("control-plane", root),
    /agent-email\/package\.json must pin.*exactly 4\.200\.0.*wrangler-version\.json/,
  );
});

test("matching manifests cannot hide a stale installed Wrangler", async (t) => {
  const root = await fixture(t);
  for (const worker of workerDirectories) {
    await writeJSON(root, `${worker}/node_modules/wrangler/package.json`, { version: "4.199.0" });
    await assert.rejects(assertWranglerVersion(worker, root), (error) => {
      assert.equal(error.code, "ERR_ASSERTION");
      assert.match(error.message, new RegExp(`${worker}/node_modules/wrangler/package\\.json`));
      assert.match(error.message, /infra\/cloudflare\/wrangler-version\.json/);
      assert.match(error.message, /Run npm ci/);
      return true;
    });
  }
});

test("a Worker gate needs only its own installed dependencies", async (t) => {
  const root = await fixture(t);
  await writeJSON(root, "agent-email/node_modules/wrangler/package.json", { version: "4.200.0" });
  await assertWranglerVersion("agent-email", root);
});

test("missing dependencies produce an actionable installation error", async (t) => {
  const root = await fixture(t);
  await assert.rejects(
    assertWranglerVersion("agent-email", root),
    /agent-email\/node_modules\/wrangler\/package\.json.*wrangler-version\.json.*npm ci/,
  );
});
