import assert from "node:assert/strict";
import { access, chmod, readFile, writeFile } from "node:fs/promises";
import test from "node:test";

import {
  createPrivateDeploymentConfig,
} from "../scripts/private-deployment-config.mjs";

test("concurrent deployment configurations are private immutable snapshots", async () => {
  const first = await createPrivateDeploymentConfig({
    prefix: "witself-config-test-",
    render: (path) => writeFile(path, "first", { mode: 0o600 }),
    validate: async (path) => assert.equal(await readFile(path, "utf8"), "first"),
  });
  const second = await createPrivateDeploymentConfig({
    prefix: "witself-config-test-",
    render: (path) => writeFile(path, "second", { mode: 0o600 }),
  });
  try {
    assert.notEqual(first.path, second.path);
    assert.equal(await first.readText(), "first");
    assert.equal(await second.readText(), "second");
    await first.assertUnchanged();
    await second.assertUnchanged();

    await chmod(first.path, 0o600);
    await writeFile(first.path, "replaced");
    await assert.rejects(
      first.assertUnchanged(),
      /unsafe metadata|changed during deployment/,
    );
    await second.assertUnchanged();
  } finally {
    await first.cleanup();
    await second.cleanup();
  }
  await assert.rejects(access(first.path), { code: "ENOENT" });
  await assert.rejects(access(second.path), { code: "ENOENT" });
});

test("failed deployment configuration rendering cleans its private directory", async () => {
  await assert.rejects(
    createPrivateDeploymentConfig({
      prefix: "witself-config-test-",
      render: async () => {
        throw new Error("render failed");
      },
    }),
    /render failed/,
  );
});
