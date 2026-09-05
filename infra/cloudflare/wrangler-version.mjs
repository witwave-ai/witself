import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

export const workerDirectories = Object.freeze([
  "control-plane",
  "agent-email",
  "agent-email-send",
  "support-email-intake",
]);

const sharedFile = "infra/cloudflare/wrangler-version.json";
const defaultRoot = fileURLToPath(new URL(".", import.meta.url));
const bumpInstructions = `Update ${sharedFile} and run npm install --save-dev ` +
  "--save-exact wrangler@<version> in all four Worker directories, then npm ci in each.";

async function readJSON(path, label) {
  try {
    return JSON.parse(await readFile(path, "utf8"));
  } catch (error) {
    throw new Error(
      `Cannot read ${label} for the Wrangler gate (${sharedFile}); ` +
      "run npm ci in the Worker directory if dependencies are missing.",
      { cause: error },
    );
  }
}

// Only this Worker's installation is needed, so its gate can run after npm ci
// in a fresh worktree without installing the other three Workers first.
export async function assertWranglerVersion(worker, cloudflareRoot = defaultRoot) {
  assert.ok(workerDirectories.includes(worker), `Unknown Cloudflare Worker: ${worker}`);
  const { version } = await readJSON(
    join(cloudflareRoot, "wrangler-version.json"), sharedFile,
  );
  assert.match(
    String(version), /^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/,
    `${sharedFile} must contain one exact stable Wrangler release (major.minor.patch).`,
  );

  for (const directory of workerDirectories) {
    const label = `infra/cloudflare/${directory}/package.json`;
    const manifest = await readJSON(join(cloudflareRoot, directory, "package.json"), label);
    assert.equal(
      manifest.devDependencies?.wrangler, version,
      `${label} must pin devDependencies.wrangler to exactly ${version} from ${sharedFile}. ` +
      bumpInstructions,
    );
  }

  const installedLabel = `infra/cloudflare/${worker}/node_modules/wrangler/package.json`;
  const installed = await readJSON(
    join(cloudflareRoot, worker, "node_modules", "wrangler", "package.json"), installedLabel,
  );
  assert.equal(
    installed.version, version,
    `${installedLabel} must match ${version} from ${sharedFile}. ` +
    `Run npm ci in infra/cloudflare/${worker}; for a release bump, ${bumpInstructions}`,
  );
}
