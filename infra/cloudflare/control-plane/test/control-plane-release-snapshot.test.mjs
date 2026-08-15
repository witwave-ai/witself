import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  access,
  chmod,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  stat,
  symlink,
  unlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import test from "node:test";

import {
  createControlPlaneReleaseSnapshot,
} from "../scripts/control-plane-release-snapshot.mjs";
import { runProductionWranglerDeploy } from
  "../scripts/deploy-release.mjs";
import { privateDeploymentConfigMain } from
  "../scripts/verify-deployment.mjs";
import { sourceIdentity } from "../scripts/source-identity.mjs";
import { PRODUCTION_CLOUDFLARE_ACCOUNT_ID } from
  "../../agent-email/scripts/wrangler-environment.mjs";

const REVIEWED_ENV =
  "# Intentionally empty: production Wrangler commands must not load local dotenv files.\n";

function git(repositoryRoot, args, environment = {}) {
  const result = spawnSync("git", args, {
    cwd: repositoryRoot,
    env: { ...process.env, ...environment },
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  assert.equal(result.status, 0, result.stderr);
  return result.stdout.trim();
}

async function write(path, value, mode = 0o644) {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, value, { mode });
}

async function createFixture() {
  const directory = await mkdtemp(join(tmpdir(), "witself-cp-release-fixture-"));
  const repositoryRoot = join(directory, "checkout");
  await mkdir(repositoryRoot);
  const packageDefinition = {
    name: "witself-control-plane-deploy",
    private: true,
    dependencies: { "@cloudflare/containers": "^0.0.28" },
  };
  const lockDefinition = {
    name: "witself-control-plane-deploy",
    lockfileVersion: 3,
    requires: true,
    packages: {
      "": packageDefinition,
      "node_modules/@cloudflare/containers": {
        version: "0.0.28",
        resolved:
          "https://registry.npmjs.org/@cloudflare/containers/-/containers-0.0.28.tgz",
        integrity: "sha512-QUFBQQ==",
      },
    },
  };
  await Promise.all([
    write(join(repositoryRoot, ".dockerignore"), [
      ".git",
      "node_modules",
      "infra/cloudflare/control-plane/node_modules",
      "infra/cloudflare/witself-control-plane-deploy-*/",
      "",
    ].join("\n")),
    write(join(repositoryRoot, ".gitignore"), [
      ".env",
      "infra/cloudflare/control-plane/node_modules/",
      "",
    ].join("\n")),
    write(
      join(repositoryRoot, "images/witself-control-plane/Dockerfile"),
      "FROM scratch\nCOPY . .\n",
    ),
    write(
      join(repositoryRoot, "infra/cloudflare/wrangler-production-empty.env"),
      REVIEWED_ENV,
    ),
    write(
      join(repositoryRoot, "infra/cloudflare/control-plane/src/index.js"),
      "export default { fetch() { return new Response('tagged'); } };\n",
    ),
    write(
      join(repositoryRoot, "infra/cloudflare/control-plane/package.json"),
      `${JSON.stringify(packageDefinition, null, 2)}\n`,
    ),
    write(
      join(repositoryRoot, "infra/cloudflare/control-plane/package-lock.json"),
      `${JSON.stringify(lockDefinition, null, 2)}\n`,
    ),
  ]);
  git(repositoryRoot, ["init", "-q"]);
  git(repositoryRoot, ["config", "user.email", "release-test@witwave.ai"]);
  git(repositoryRoot, ["config", "user.name", "Release Test"]);
  git(repositoryRoot, ["add", "."]);
  git(repositoryRoot, ["commit", "-qm", "release fixture"], {
    GIT_AUTHOR_DATE: "2026-08-14T12:00:00Z",
    GIT_COMMITTER_DATE: "2026-08-14T12:00:00Z",
  });
  git(repositoryRoot, ["tag", "v1.2.3"]);

  const installedDependencyRoot = join(
    repositoryRoot,
    "infra/cloudflare/control-plane/node_modules/@cloudflare/containers",
  );
  await Promise.all([
    write(join(installedDependencyRoot, "package.json"), `${JSON.stringify({
      name: "@cloudflare/containers",
      version: "0.0.28",
      exports: { ".": { import: "./dist/index.js" } },
      files: ["dist"],
    }, null, 2)}\n`),
    write(
      join(installedDependencyRoot, "dist/index.js"),
      "export class Container {}\nexport const getContainer = () => null;\n",
    ),
  ]);
  return {
    directory,
    repositoryRoot,
    installedDependencyRoot,
    identity: sourceIdentity({ repositoryRoot }),
  };
}

function renderedConfig() {
  return `${JSON.stringify({
    name: "witself-control-plane",
    main: "src/index.js",
    containers: [{
      image: "../../../images/witself-control-plane/Dockerfile",
      image_build_context: "../../..",
    }],
  }, null, 2)}\n`;
}

test("tagged control-plane snapshot freezes Worker, dependency, and Docker context", async () => {
  const fixture = await createFixture();
  let snapshot;
  try {
    snapshot = await createControlPlaneReleaseSnapshot({
      identity: fixture.identity,
      repositoryRoot: fixture.repositoryRoot,
      installedDependencyRoot: fixture.installedDependencyRoot,
      render: (path) => write(path, renderedConfig(), 0o600),
    });
    assert.match(snapshot.inventory.sha256, /^[0-9a-f]{64}$/);
    assert.ok(snapshot.inventory.file_count >= 9);
    assert.ok(snapshot.inventory.byte_count > 0);
    assert.equal(
      relative(fixture.repositoryRoot, snapshot.repositoryRoot).startsWith(".."),
      true,
    );
    assert.equal((await stat(snapshot.repositoryRoot)).mode & 0o777, 0o555);
    assert.equal((await stat(snapshot.entrypointTarget)).mode & 0o777, 0o444);
    assert.equal(
      await readFile(snapshot.entrypointTarget, "utf8"),
      "export default { fetch() { return new Response('tagged'); } };\n",
    );

    const config = JSON.parse(await snapshot.readText());
    const configDirectory = dirname(snapshot.path);
    assert.equal(config.main, "../control-plane/src/index.js");
    assert.equal(resolve(configDirectory, config.main), snapshot.entrypointTarget);
    assert.equal(
      resolve(configDirectory, config.containers[0].image),
      join(snapshot.repositoryRoot, "images/witself-control-plane/Dockerfile"),
    );
    assert.equal(
      resolve(configDirectory, config.containers[0].image_build_context),
      snapshot.repositoryRoot,
    );
    assert.equal(
      privateDeploymentConfigMain(snapshot.path, snapshot.controlPlaneRoot),
      "../control-plane/src/index.js",
    );
    assert.equal(
      await readFile(join(
        snapshot.controlPlaneRoot,
        "node_modules/@cloudflare/containers/dist/index.js",
      ), "utf8"),
      "export class Container {}\nexport const getContainer = () => null;\n",
    );

    await Promise.all([
      writeFile(
        join(fixture.repositoryRoot, "infra/cloudflare/control-plane/src/index.js"),
        "throw new Error('live checkout poison');\n",
      ),
      writeFile(
        join(fixture.repositoryRoot, "images/witself-control-plane/Dockerfile"),
        "FROM attacker.invalid/image\n",
      ),
      writeFile(
        join(fixture.installedDependencyRoot, "dist/index.js"),
        "throw new Error('live dependency poison');\n",
      ),
      writeFile(join(fixture.repositoryRoot, ".env"), "SECRET=poison\n"),
    ]);
    await snapshot.assertUnchanged();
    await assert.rejects(
      access(join(snapshot.repositoryRoot, ".env")),
      { code: "ENOENT" },
    );
    let wranglerCalls = 0;
    await runProductionWranglerDeploy(
      ["deploy", "--config", snapshot.path, "--strict"],
      {
        environment: {
          PATH: process.env.PATH,
          CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
          CLOUDFLARE_API_TOKEN: "canonical-production-token",
        },
        cwd: snapshot.workDirectory,
        reviewedEnvironmentFile: snapshot.reviewedEnvironmentFile,
        runCommand: async (command, args, options) => {
          wranglerCalls += 1;
          assert.equal(command, "wrangler");
          assert.equal(options.cwd, snapshot.workDirectory);
          assert.deepEqual(args.slice(-2), [
            "--env-file", snapshot.reviewedEnvironmentFile,
          ]);
          const configPath = args[args.indexOf("--config") + 1];
          assert.equal(configPath, snapshot.path);
          const frozenConfig = JSON.parse(await readFile(configPath, "utf8"));
          assert.equal(
            await readFile(resolve(dirname(configPath), frozenConfig.main), "utf8"),
            "export default { fetch() { return new Response('tagged'); } };\n",
          );
          assert.equal(
            resolve(
              dirname(configPath),
              frozenConfig.containers[0].image_build_context,
            ),
            snapshot.repositoryRoot,
          );
        },
      },
    );
    assert.equal(wranglerCalls, 1);

    const extraConfigFile = join(dirname(snapshot.path), ".env");
    await writeFile(extraConfigFile, "CLOUDFLARE_ACCOUNT_ID=attacker\n");
    await assert.rejects(
      snapshot.assertUnchanged(),
      /private deployment configuration had unsafe metadata/,
    );
    await unlink(extraConfigFile);
    await snapshot.assertUnchanged();

    await chmod(snapshot.entrypointTarget, 0o600);
    await writeFile(snapshot.entrypointTarget, "changed snapshot\n");
    await assert.rejects(
      snapshot.assertUnchanged(),
      /release snapshot changed during deployment/,
    );
  } finally {
    const snapshotRoot = snapshot?.directory;
    await snapshot?.cleanup();
    if (snapshotRoot) {
      await assert.rejects(access(snapshotRoot), { code: "ENOENT" });
    }
    await rm(fixture.directory, { recursive: true, force: true });
  }
});

test("unsafe repository export fails closed and removes its external snapshot", async () => {
  const fixture = await createFixture();
  let snapshotRoot = "";
  try {
    await assert.rejects(
      createControlPlaneReleaseSnapshot({
        identity: fixture.identity,
        repositoryRoot: fixture.repositoryRoot,
        installedDependencyRoot: fixture.installedDependencyRoot,
        exportRepository: async (_source, destination) => {
          snapshotRoot = dirname(destination);
          await Promise.all([
            write(join(destination, ".dockerignore"), [
              "node_modules",
              "infra/cloudflare/control-plane/node_modules",
              "infra/cloudflare/witself-control-plane-deploy-*/",
              "",
            ].join("\n")),
            write(
              join(destination, "images/witself-control-plane/Dockerfile"),
              "FROM scratch\n",
            ),
            write(
              join(destination, "infra/cloudflare/wrangler-production-empty.env"),
              REVIEWED_ENV,
            ),
            write(
              join(destination, "infra/cloudflare/control-plane/src/index.js"),
              "export default {};\n",
            ),
            write(
              join(destination, "infra/cloudflare/control-plane/package.json"),
              await readFile(join(
                fixture.repositoryRoot,
                "infra/cloudflare/control-plane/package.json",
              )),
            ),
            write(
              join(destination, "infra/cloudflare/control-plane/package-lock.json"),
              await readFile(join(
                fixture.repositoryRoot,
                "infra/cloudflare/control-plane/package-lock.json",
              )),
            ),
          ]);
          await symlink(
            "index.js",
            join(destination, "infra/cloudflare/control-plane/src/escape.mjs"),
          );
        },
        render: (path) => write(path, renderedConfig(), 0o600),
      }),
      /non-file entry/,
    );
    assert.notEqual(snapshotRoot, "");
    await assert.rejects(access(snapshotRoot), { code: "ENOENT" });
  } finally {
    await rm(fixture.directory, { recursive: true, force: true });
  }
});

test("failed snapshot config rendering removes the complete external build context", async () => {
  const fixture = await createFixture();
  let snapshotRoot = "";
  try {
    await assert.rejects(
      createControlPlaneReleaseSnapshot({
        identity: fixture.identity,
        repositoryRoot: fixture.repositoryRoot,
        installedDependencyRoot: fixture.installedDependencyRoot,
        render: async (_path, layout) => {
          snapshotRoot = layout.directory;
          throw new Error("simulated snapshot render failure");
        },
      }),
      /simulated snapshot render failure/,
    );
    assert.notEqual(snapshotRoot, "");
    await assert.rejects(access(snapshotRoot), { code: "ENOENT" });
  } finally {
    await rm(fixture.directory, { recursive: true, force: true });
  }
});
