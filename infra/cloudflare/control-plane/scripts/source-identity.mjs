import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const controlPlaneRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const defaultRepositoryRoot = join(controlPlaneRoot, "..", "..", "..");

const RELEASE_TAG = /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;
const FULL_COMMIT = /^[0-9a-f]{40}$/;

function runGit(args, repositoryRoot) {
  const result = spawnSync("git", args, {
    cwd: repositoryRoot,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  return {
    ok: !result.error && result.status === 0,
    stdout: String(result.stdout ?? "").trim(),
  };
}

function requiredGit(args, repositoryRoot, operation) {
  const result = runGit(args, repositoryRoot);
  if (!result.ok || !result.stdout || /[\r\n\0]/.test(result.stdout)) {
    throw new Error(`could not resolve ${operation} from git`);
  }
  return result.stdout;
}

export function validReleaseDate(value) {
  return typeof value === "string" && value.length <= 64 &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) &&
    !Number.isNaN(Date.parse(value));
}

export function validateBuildMetadata(metadata) {
  if (!metadata || typeof metadata !== "object" ||
      !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(
        String(metadata.version ?? ""),
      )) {
    throw new Error("version must be MAJOR.MINOR.PATCH without a v prefix");
  }
  if (!FULL_COMMIT.test(String(metadata.commit ?? ""))) {
    throw new Error("commit must be a full lowercase Git SHA");
  }
  if (!validReleaseDate(metadata.date)) {
    throw new Error("date must be an RFC3339 timestamp");
  }
  return Object.freeze({
    version: metadata.version,
    commit: metadata.commit,
    date: metadata.date,
  });
}

export function assertReleaseSource(identity) {
  let metadata;
  try {
    metadata = validateBuildMetadata(identity);
  } catch {
    throw new Error(
      "control-plane deployment requires a clean checkout at one exact semantic-version tag",
    );
  }
  if (identity.clean !== true || identity.tag !== `v${metadata.version}` ||
      !RELEASE_TAG.test(identity.tag)) {
    throw new Error(
      "control-plane deployment requires a clean checkout at one exact semantic-version tag",
    );
  }
  return Object.freeze({ ...metadata, tag: identity.tag, clean: true });
}

export function sourceIdentity({ repositoryRoot = defaultRepositoryRoot } = {}) {
  const commit = requiredGit(
    ["rev-parse", "HEAD"],
    repositoryRoot,
    "source commit",
  );
  const date = requiredGit(
    ["show", "--no-show-signature", "-s", "--format=%cI", "HEAD"],
    repositoryRoot,
    "source commit date",
  );
  const status = runGit(
    ["status", "--porcelain=v1", "--untracked-files=all"],
    repositoryRoot,
  );
  if (!status.ok) throw new Error("could not determine source tree state from git");

  const pointedTags = runGit(
    ["tag", "--points-at", "HEAD", "--list", "v*"],
    repositoryRoot,
  );
  if (!pointedTags.ok) throw new Error("could not determine source release tag from git");
  const semanticTags = pointedTags.stdout
    .split("\n")
    .map((value) => value.trim())
    .filter((value) => RELEASE_TAG.test(value));
  const tag = semanticTags.length === 1 ? semanticTags[0] : "";

  return assertReleaseSource({
    version: tag ? tag.slice(1) : "",
    commit,
    date,
    tag,
    clean: status.stdout === "",
  });
}

// Resolve a historical release through its exact annotated/lightweight tag.
// Deployment bootstrap uses this to prove the currently active predecessor is
// the one immutable release immediately before the lease endpoint existed; a
// version string by itself is not sufficient authority for that exception.
export function taggedReleaseIdentity(
  version,
  { repositoryRoot = defaultRepositoryRoot } = {},
) {
  if (!/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(
    String(version ?? ""),
  )) {
    throw new Error("historical release version must be MAJOR.MINOR.PATCH");
  }
  const tag = `v${version}`;
  const commit = requiredGit(
    ["rev-parse", "--verify", `${tag}^{commit}`],
    repositoryRoot,
    `${tag} commit`,
  );
  const date = requiredGit(
    ["show", "--no-show-signature", "-s", "--format=%cI", commit],
    repositoryRoot,
    `${tag} commit date`,
  );
  return Object.freeze({
    ...validateBuildMetadata({ version, commit, date }),
    tag,
  });
}

export function workerVersionTag(metadata) {
  return `v${validateBuildMetadata(metadata).version}`;
}

export function workerVersionMessage(metadata) {
  const release = validateBuildMetadata(metadata);
  return `witself-control-plane v${release.version} ${release.commit}`;
}
