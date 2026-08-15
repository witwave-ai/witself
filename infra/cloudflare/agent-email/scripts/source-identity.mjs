import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const edgeRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
const defaultRepositoryRoot = join(edgeRoot, "..", "..", "..");

const RELEASE_TAG = /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;
const FULL_COMMIT = /^[0-9a-f]{40}$/;
const SAFE_DEVELOPMENT_VERSION = /^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$/;

export function sanitizedGitEnvironment(source = process.env) {
  const environment = { ...source };
  for (const name of Object.keys(environment)) {
    if (name.startsWith("GIT_") || name === "SSH_ASKPASS" ||
        name === "GIT_ASKPASS") {
      delete environment[name];
    }
  }
  Object.assign(environment, {
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_NO_REPLACE_OBJECTS: "1",
    GIT_TERMINAL_PROMPT: "0",
  });
  return environment;
}

function runGit(args, repositoryRoot, environment) {
  const result = spawnSync("git", args, {
    cwd: repositoryRoot,
    env: sanitizedGitEnvironment(environment),
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  return {
    ok: !result.error && result.status === 0,
    stdout: String(result.stdout ?? "").trim(),
  };
}

function requiredGit(args, repositoryRoot, operation, environment) {
  const result = runGit(args, repositoryRoot, environment);
  if (!result.ok || !result.stdout || /[\r\n\0]/.test(result.stdout)) {
    throw new Error(`could not resolve ${operation} from git`);
  }
  return result.stdout;
}

function validCommitDate(value) {
  return typeof value === "string" && value.length <= 64 &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) &&
    !Number.isNaN(Date.parse(value));
}

export function assertReleaseSource(identity) {
  if (!identity || typeof identity !== "object" ||
      !FULL_COMMIT.test(String(identity.commit ?? "")) ||
      !validCommitDate(identity.date) || identity.clean !== true ||
      !RELEASE_TAG.test(String(identity.tag ?? "")) ||
      identity.version !== identity.tag.slice(1)) {
    throw new Error(
      "email Worker deployment requires a clean checkout at one exact semantic-version tag",
    );
  }
  return identity;
}

export function sourceIdentity({
  repositoryRoot = defaultRepositoryRoot,
  requireRelease = false,
  environment = process.env,
} = {}) {
  const commit = requiredGit(
    ["rev-parse", "HEAD"],
    repositoryRoot,
    "source commit",
    environment,
  );
  if (!FULL_COMMIT.test(commit)) {
    throw new Error("git returned an invalid source commit");
  }
  const date = requiredGit(
    ["show", "-s", "--format=%cI", "HEAD"],
    repositoryRoot,
    "source commit date",
    environment,
  );
  if (!validCommitDate(date)) {
    throw new Error("git returned an invalid source commit date");
  }

  const status = runGit(
    ["status", "--porcelain=v1", "--untracked-files=all"],
    repositoryRoot,
    environment,
  );
  if (!status.ok) throw new Error("could not determine source tree state from git");
  const clean = status.stdout === "";

  const pointedTags = runGit(
    ["tag", "--points-at", "HEAD", "--list", "v[0-9]*"],
    repositoryRoot,
    environment,
  );
  if (!pointedTags.ok) throw new Error("could not determine source release tag from git");
  const semanticTags = pointedTags.stdout
    .split("\n")
    .map((value) => value.trim())
    .filter((value) => RELEASE_TAG.test(value));
  const tag = semanticTags.length === 1 ? semanticTags[0] : "";
  const version = tag && clean
    ? tag.slice(1)
    : `development-${commit.slice(0, 12)}${clean ? "" : "-dirty"}`;
  if (!SAFE_DEVELOPMENT_VERSION.test(version)) {
    throw new Error("could not derive a safe email Worker source version");
  }

  const identity = Object.freeze({ version, commit, date, tag, clean });
  return requireRelease ? assertReleaseSource(identity) : identity;
}
