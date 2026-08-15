import assert from "node:assert/strict";
import {
  access,
  chmod,
  lstat,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  createPrivateDeploymentConfig,
} from "../scripts/private-deployment-config.mjs";

const repoRoot = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../../../..",
);

test("root Docker context excludes every private Wrangler snapshot", async () => {
  const lines = (await readFile(join(repoRoot, ".dockerignore"), "utf8"))
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith("#"));
  const prefixes = [
    "witself-control-plane-deploy-*/",
    "witself-agent-email-deploy-*/",
    "witself-relay-control-plane-*/",
    "witself-relay-email-edge-*/",
  ];
  for (const prefix of prefixes) {
    assert.ok(
      lines.includes(`infra/cloudflare/${prefix}`),
      `missing repo-relative Docker ignore for ${prefix}`,
    );
    assert.ok(
      !lines.includes(`/${prefix}`),
      `root-only Docker ignore cannot match infra/cloudflare/${prefix}`,
    );
  }
});

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

test("private deployment configuration rejects residual Wrangler deploy state", async () => {
  const config = await createPrivateDeploymentConfig({
    prefix: "witself-config-test-",
    render: (path) => writeFile(path, "config", { mode: 0o600 }),
  });
  const residue = join(
    dirname(config.path),
    ".wrangler",
    "tmp",
    "deploy-regression",
  );
  try {
    await mkdir(residue, { mode: 0o700 });
    await assert.rejects(
      config.assertUnchanged(),
      /unsafe metadata/,
    );
    await rm(residue, { recursive: true, force: true });
    await config.assertUnchanged();
  } finally {
    await config.cleanup();
  }
});

test("sibling deployment snapshots preserve Wrangler path resolution and clean up", async () => {
  const fixture = await mkdtemp(join(tmpdir(), "witself-config-layout-test-"));
  const cloudflare = join(fixture, "infra", "cloudflare");
  const entrypoint = join(cloudflare, "control-plane", "src", "index.js");
  const dockerfile = join(
    fixture,
    "images",
    "witself-control-plane",
    "Dockerfile",
  );
  await mkdir(dirname(entrypoint), { recursive: true });
  await mkdir(dirname(dockerfile), { recursive: true });
  await writeFile(entrypoint, "export default {};\n");
  await writeFile(dockerfile, "FROM scratch\n");

  let config;
  try {
    config = await createPrivateDeploymentConfig({
      prefix: "witself-control-plane-deploy-",
      parentDirectory: cloudflare,
      entrypointTarget: entrypoint,
      render: (path) => writeFile(path, `${JSON.stringify({
        name: "witself-control-plane",
        main: "src/index.js",
        containers: [{
          image: "../../../images/witself-control-plane/Dockerfile",
          image_build_context: "../../..",
        }],
      }, null, 2)}\n`, { mode: 0o600 }),
    });

    const configDirectory = dirname(config.path);
    assert.match(
      relative(cloudflare, configDirectory),
      /^witself-control-plane-deploy-[^/]+$/,
    );
    assert.equal((await lstat(configDirectory)).mode & 0o777, 0o700);
    assert.equal((await lstat(config.path)).mode & 0o777, 0o400);

    const rendered = JSON.parse(await config.readText());
    assert.equal(rendered.main, "../control-plane/src/index.js");
    assert.equal(resolve(configDirectory, rendered.main), entrypoint);
    assert.equal(
      rendered.containers[0].image,
      "../../../images/witself-control-plane/Dockerfile",
    );
    assert.equal(
      resolve(configDirectory, rendered.containers[0].image),
      dockerfile,
    );
    assert.equal(rendered.containers[0].image_build_context, "../../..");
    assert.equal(
      resolve(configDirectory, rendered.containers[0].image_build_context),
      fixture,
    );

    await chmod(configDirectory, 0o755);
    await assert.rejects(
      config.assertUnchanged(),
      /unsafe metadata/,
    );
    await chmod(configDirectory, 0o700);
    await config.assertUnchanged();

    await config.cleanup();
    await assert.rejects(access(configDirectory), { code: "ENOENT" });
  } finally {
    await config?.cleanup();
    await rm(fixture, { recursive: true, force: true });
  }
});

test("relocation rejects incomplete, external, and ambiguous entrypoints without residue", async () => {
  const fixture = await mkdtemp(join(tmpdir(), "witself-config-options-test-"));
  const parent = join(fixture, "infra", "cloudflare");
  const entrypoint = join(parent, "control-plane", "src", "index.js");
  const external = join(fixture, "outside.js");
  await mkdir(dirname(entrypoint), { recursive: true });
  await writeFile(entrypoint, "export default {};\n");
  await writeFile(external, "export default {};\n");
  try {
    for (const options of [
      { parentDirectory: parent },
      { entrypointTarget: entrypoint },
      { parentDirectory: parent, entrypointTarget: external },
    ]) {
      await assert.rejects(
        createPrivateDeploymentConfig({
          prefix: "witself-config-test-",
          render: (path) => writeFile(
            path,
            '{"main": "src/index.js"}\n',
            { mode: 0o600 },
          ),
          ...options,
        }),
        /requires both|inside its parent directory/,
      );
    }

    for (const rendered of [
      '{"name": "missing-main"}\n',
      '{"main": "src/index.js", "copy": {"main": "src/index.js"}}\n',
    ]) {
      let renderedPath = "";
      await assert.rejects(
        createPrivateDeploymentConfig({
          prefix: "witself-config-test-",
          parentDirectory: parent,
          entrypointTarget: entrypoint,
          render: async (path) => {
            renderedPath = path;
            await writeFile(path, rendered, { mode: 0o600 });
          },
        }),
        /exact Wrangler main entrypoint once/,
      );
      assert.notEqual(renderedPath, "");
      await assert.rejects(access(dirname(renderedPath)), { code: "ENOENT" });
    }
  } finally {
    await rm(fixture, { recursive: true, force: true });
  }
});

test("failed deployment configuration rendering cleans its private directory", async () => {
  const fixture = await mkdtemp(join(tmpdir(), "witself-config-failure-test-"));
  const entrypoint = join(fixture, "src", "index.js");
  await mkdir(dirname(entrypoint), { recursive: true });
  await writeFile(entrypoint, "export default {};\n");
  let renderedPath = "";
  try {
    await assert.rejects(
      createPrivateDeploymentConfig({
        prefix: "witself-config-test-",
        parentDirectory: fixture,
        entrypointTarget: entrypoint,
        render: async (path) => {
          renderedPath = path;
          throw new Error("render failed");
        },
      }),
      /render failed/,
    );
    assert.notEqual(renderedPath, "");
    await assert.rejects(access(dirname(renderedPath)), { code: "ENOENT" });
  } finally {
    await rm(fixture, { recursive: true, force: true });
  }
});
