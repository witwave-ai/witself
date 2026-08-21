#!/usr/bin/env node
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";

import {
  BROKER_SERVER_NAME,
  CEILING_ENV,
  GRANT_KEY_FILE_ENV,
  normalizeLauncherMode,
  requireProfile,
} from "../.claude/codex-profiles.mjs";

const SCRIPT_PATH = fileURLToPath(import.meta.url);
const PROJECT_ROOT = fs.realpathSync(path.resolve(path.dirname(SCRIPT_PATH), ".."));
const BROKER_SOURCE_ROOT = path.join(PROJECT_ROOT, "tools", "claude-codex-broker");
const BROKER_PATH = path.join(BROKER_SOURCE_ROOT, "server.mjs");
const PROFILE_PATH = path.join(PROJECT_ROOT, ".claude", "codex-profiles.mjs");
const HOOK_PATH = path.join(PROJECT_ROOT, ".claude", "hooks", "codex-ceiling-guard.mjs");
const SESSION_PREFIX = "witself-claude-codex-";
const MINIMUM_CLAUDE_VERSION = Object.freeze([2, 1, 214]);
const SNAPSHOT_MAX_FILES = 1_024;
const SNAPSHOT_MAX_BYTES = 32 * 1024 * 1024;
// Claude treats --settings as additive. Passing an explicit empty ordinary
// source list excludes mutable user, project, and local settings while keeping
// the launcher-generated settings file below. Admin-managed policy remains a
// host trust boundary enforced by Claude Code itself.
const PRIVATE_SETTING_SOURCES = "";

const MANAGED_CLAUDE_FLAGS = Object.freeze([
  "--mcp-config",
  "--strict-mcp-config",
  "--permission-mode",
  "--dangerously-skip-permissions",
  "--allow-dangerously-skip-permissions",
  "--settings",
  "--setting-sources",
  "--safe-mode",
  "--bare",
]);

function requireCanonicalRegularFile(filePath, label, { executable = false } = {}) {
  if (!path.isAbsolute(filePath) || fs.realpathSync(filePath) !== filePath) {
    throw new Error(`${label} must use a canonical absolute path.`);
  }
  const stat = fs.lstatSync(filePath);
  if (!stat.isFile() || stat.isSymbolicLink()) throw new Error(`${label} must be a regular file, not a link.`);
  if (typeof process.getuid === "function" && stat.uid !== process.getuid()) {
    throw new Error(`${label} must be owned by the current user.`);
  }
  if (executable && (stat.mode & 0o111) === 0) throw new Error(`${label} must be executable.`);
  return stat;
}

function executableIdentity(filePath, stat) {
  return Object.freeze({
    path: filePath,
    dev: String(stat.dev),
    ino: String(stat.ino),
    mode: stat.mode,
    size: stat.size,
    mtimeMs: stat.mtimeMs,
    ctimeMs: stat.ctimeMs,
  });
}

function verifyExecutableIdentity(identity) {
  const stat = requireCanonicalRegularFile(identity.path, "The Claude Code executable", { executable: true });
  if (String(stat.dev) !== identity.dev || String(stat.ino) !== identity.ino || stat.mode !== identity.mode ||
      stat.size !== identity.size || stat.mtimeMs !== identity.mtimeMs || stat.ctimeMs !== identity.ctimeMs) {
    throw new Error("The Claude Code executable changed after launcher verification.");
  }
}

export function resolveClaudeExecutable(configured, environment = process.env) {
  const requested = configured ?? "claude";
  if (typeof requested !== "string" || requested.length === 0 || requested.length > 4096 || /[\0\r\n]/u.test(requested)) {
    throw new Error("The Claude Code executable setting is invalid.");
  }
  const candidates = [];
  if (path.isAbsolute(requested)) {
    candidates.push(requested);
  } else {
    if (requested !== path.basename(requested)) {
      throw new Error("CLAUDE_CODE_EXECUTABLE must be a canonical absolute path when it includes a directory.");
    }
    const searchPath = environment.PATH;
    if (typeof searchPath !== "string" || searchPath.length === 0 || searchPath.length > 64 * 1024) {
      throw new Error("PATH is unavailable for resolving Claude Code.");
    }
    const extensions = process.platform === "win32"
      ? (environment.PATHEXT || ".EXE;.CMD;.BAT;.COM").split(";").filter(Boolean)
      : [""];
    for (const directory of searchPath.split(path.delimiter)) {
      if (!directory || !path.isAbsolute(directory)) continue;
      for (const extension of extensions) candidates.push(path.join(directory, `${requested}${extension}`));
    }
  }
  for (const candidate of candidates) {
    try {
      fs.accessSync(candidate, fs.constants.X_OK);
      const canonical = fs.realpathSync(candidate);
      const stat = requireCanonicalRegularFile(canonical, "The Claude Code executable", { executable: true });
      return executableIdentity(canonical, stat);
    } catch {}
  }
  throw new Error("Could not resolve Claude Code to a canonical executable file.");
}

export function validateProjectSurface(projectRoot = PROJECT_ROOT) {
  const root = path.resolve(projectRoot);
  if (!path.isAbsolute(root) || fs.realpathSync(root) !== root) {
    throw new Error("The Claude Codex launcher requires a canonical project root.");
  }
  requireCanonicalRegularFile(SCRIPT_PATH, "The Claude Codex launcher", { executable: true });
  requireCanonicalRegularFile(path.join(root, "tools", "claude-codex-broker", "server.mjs"), "The Codex broker");
  requireCanonicalRegularFile(path.join(root, ".claude", "codex-profiles.mjs"), "The Codex profile and grant signer");
  requireCanonicalRegularFile(path.join(root, ".claude", "hooks", "codex-ceiling-guard.mjs"), "The Codex grant hook");
  requireCanonicalRegularFile(process.execPath, "The Node.js runtime", { executable: true });

  const git = process.platform === "win32" ? "git.exe" : "/usr/bin/git";
  const result = spawnSync(git, ["-C", root, "rev-parse", "--show-toplevel"], {
    encoding: "utf8",
    shell: false,
    env: process.platform === "win32"
      ? { SystemRoot: process.env.SystemRoot, PATH: process.env.PATH }
      : { PATH: "/usr/bin:/bin", LANG: "C", LC_ALL: "C", GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null" },
  });
  if (result.error || result.status !== 0) throw new Error("The launcher could not verify the project Git root.");
  const reportedRoot = result.stdout.trim();
  if (path.resolve(reportedRoot) !== root || fs.realpathSync(reportedRoot) !== root) {
    throw new Error("The launcher path does not exactly match git rev-parse --show-toplevel.");
  }
  return Object.freeze({ projectRoot: root, brokerPath: path.join(root, "tools", "claude-codex-broker", "server.mjs") });
}

function stableStatFields(stat) {
  return {
    dev: stat.dev,
    ino: stat.ino,
    size: stat.size,
    mode: stat.mode,
    mtimeNs: stat.mtimeNs,
    ctimeNs: stat.ctimeNs,
  };
}

function sameStableStat(left, right) {
  return left.dev === right.dev && left.ino === right.ino && left.size === right.size &&
    left.mode === right.mode && left.mtimeNs === right.mtimeNs && left.ctimeNs === right.ctimeNs;
}

function readStableRegularFile(filePath) {
  if (!path.isAbsolute(filePath) || fs.realpathSync(filePath) !== filePath) {
    throw new Error(`Runtime snapshot source is not canonical: ${filePath}`);
  }
  const before = fs.lstatSync(filePath, { bigint: true });
  if (!before.isFile() || before.isSymbolicLink()) throw new Error(`Runtime snapshot source is not a regular file: ${filePath}`);
  const noFollow = fs.constants.O_NOFOLLOW ?? 0;
  const descriptor = fs.openSync(filePath, fs.constants.O_RDONLY | noFollow);
  try {
    const openedBefore = fs.fstatSync(descriptor, { bigint: true });
    if (!sameStableStat(stableStatFields(before), stableStatFields(openedBefore))) {
      throw new Error(`Runtime snapshot source changed before it could be read: ${filePath}`);
    }
    const bytes = fs.readFileSync(descriptor);
    const openedAfter = fs.fstatSync(descriptor, { bigint: true });
    const after = fs.lstatSync(filePath, { bigint: true });
    if (!sameStableStat(stableStatFields(openedBefore), stableStatFields(openedAfter)) ||
        !sameStableStat(stableStatFields(openedAfter), stableStatFields(after)) ||
        fs.realpathSync(filePath) !== filePath || BigInt(bytes.length) !== openedAfter.size) {
      throw new Error(`Runtime snapshot source changed while it was being read: ${filePath}`);
    }
    return Object.freeze({
      bytes,
      size: bytes.length,
      digest: crypto.createHash("sha256").update(bytes).digest("hex"),
      identity: Object.freeze({ dev: String(after.dev), ino: String(after.ino) }),
      mode: Number(after.mode & 0o7777n),
    });
  } finally {
    fs.closeSync(descriptor);
  }
}

function safeSnapshotRelative(relative) {
  if (typeof relative !== "string" || relative.length === 0 || relative.length > 4096 ||
      path.isAbsolute(relative) || /[\0\r\n]/u.test(relative)) return false;
  const normalized = path.normalize(relative);
  return normalized === relative && normalized !== ".." && !normalized.startsWith(`..${path.sep}`);
}

function collectSnapshotTree(sourceRoot, destinationPrefix, files, budget) {
  if (fs.realpathSync(sourceRoot) !== sourceRoot) throw new Error(`Runtime snapshot directory is not canonical: ${sourceRoot}`);
  const walk = (directory, relativeDirectory) => {
    const before = fs.lstatSync(directory, { bigint: true });
    if (!before.isDirectory() || before.isSymbolicLink()) throw new Error(`Runtime snapshot contains an unsafe directory: ${directory}`);
    const names = fs.readdirSync(directory).sort();
    for (const name of names) {
      if (name === "." || name === ".." || /[\0\r\n]/u.test(name)) throw new Error("Runtime snapshot contains an unsafe path name.");
      const source = path.join(directory, name);
      const relative = relativeDirectory ? path.join(relativeDirectory, name) : name;
      const stat = fs.lstatSync(source);
      if (stat.isSymbolicLink()) throw new Error(`Runtime snapshot refuses symbolic links: ${source}`);
      if (stat.isDirectory()) {
        if (fs.realpathSync(source) !== source) throw new Error(`Runtime snapshot directory is not canonical: ${source}`);
        walk(source, relative);
      } else if (stat.isFile()) {
        const destination = path.join(destinationPrefix, relative);
        if (!safeSnapshotRelative(destination)) throw new Error("Runtime snapshot destination is unsafe.");
        const captured = readStableRegularFile(source);
        budget.files += 1;
        budget.bytes += captured.size;
        if (budget.files > SNAPSHOT_MAX_FILES || budget.bytes > SNAPSHOT_MAX_BYTES) {
          throw new Error("The broker runtime surface exceeds the bounded launcher snapshot.");
        }
        files.push({ source, destination, captured });
      } else {
        throw new Error(`Runtime snapshot refuses special files: ${source}`);
      }
    }
    const after = fs.lstatSync(directory, { bigint: true });
    if (before.dev !== after.dev || before.ino !== after.ino || before.mtimeNs !== after.mtimeNs || before.ctimeNs !== after.ctimeNs) {
      throw new Error(`Runtime snapshot directory changed while it was enumerated: ${directory}`);
    }
  };
  walk(sourceRoot, "");
}

function collectSnapshotFile(source, destination, files, budget) {
  if (!safeSnapshotRelative(destination)) throw new Error("Runtime snapshot destination is unsafe.");
  const captured = readStableRegularFile(source);
  budget.files += 1;
  budget.bytes += captured.size;
  if (budget.files > SNAPSHOT_MAX_FILES || budget.bytes > SNAPSHOT_MAX_BYTES) {
    throw new Error("The broker runtime surface exceeds the bounded launcher snapshot.");
  }
  files.push({ source, destination, captured });
}

function ensureSnapshotDirectory(sessionDir, relative, directoryIdentities) {
  if (!safeSnapshotRelative(relative)) throw new Error("Runtime snapshot directory is unsafe.");
  const destination = path.join(sessionDir, relative);
  const containment = path.relative(sessionDir, destination);
  if (!safeSnapshotRelative(containment)) throw new Error("Runtime snapshot directory escaped its private session.");
  fs.mkdirSync(destination, { mode: 0o700 });
  fs.chmodSync(destination, 0o700);
  const stat = fs.lstatSync(destination);
  if (!stat.isDirectory() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0) {
    throw new Error("Could not create a private runtime snapshot directory.");
  }
  directoryIdentities.set(destination, identityOf(stat));
}

export function verifyRuntimeSnapshot(snapshot) {
  for (const directory of snapshot.directories) {
    const stat = fs.lstatSync(directory.path);
    if (!sameIdentity(stat, directory.identity) || !stat.isDirectory() || stat.isSymbolicLink() || (stat.mode & 0o222) !== 0) {
      throw new Error(`Frozen runtime directory changed: ${directory.path}`);
    }
  }
  for (const file of snapshot.files) {
    const captured = readStableRegularFile(file.path);
    if (!sameIdentity({ dev: captured.identity.dev, ino: captured.identity.ino }, file.identity) ||
        captured.size !== file.size || captured.digest !== file.digest || (captured.mode & 0o222) !== 0) {
      throw new Error(`Frozen runtime file changed: ${file.path}`);
    }
  }
  return true;
}

export function snapshotRuntimeSurface({ sessionDir, identities, directoryIdentities }) {
  const sourceFiles = [];
  const budget = { files: 0, bytes: 0 };
  collectSnapshotTree(BROKER_SOURCE_ROOT, path.join("tools", "claude-codex-broker"), sourceFiles, budget);
  collectSnapshotFile(PROFILE_PATH, path.join(".claude", "codex-profiles.mjs"), sourceFiles, budget);
  collectSnapshotFile(HOOK_PATH, path.join(".claude", "hooks", "codex-ceiling-guard.mjs"), sourceFiles, budget);
  sourceFiles.sort((left, right) => left.destination.localeCompare(right.destination));
  for (const file of sourceFiles) {
    const current = readStableRegularFile(file.source);
    if (current.identity.dev !== file.captured.identity.dev || current.identity.ino !== file.captured.identity.ino ||
        current.size !== file.captured.size || current.digest !== file.captured.digest || current.mode !== file.captured.mode) {
      throw new Error(`Broker runtime source changed during snapshot: ${file.source}`);
    }
  }
  const destinations = new Set();
  for (const file of sourceFiles) {
    if (destinations.has(file.destination)) throw new Error(`Duplicate runtime snapshot path: ${file.destination}`);
    destinations.add(file.destination);
  }

  const relativeDirectories = new Set();
  for (const file of sourceFiles) {
    let directory = path.dirname(file.destination);
    while (directory !== ".") {
      relativeDirectories.add(directory);
      directory = path.dirname(directory);
    }
  }
  const orderedDirectories = [...relativeDirectories].sort((left, right) => {
    const depth = left.split(path.sep).length - right.split(path.sep).length;
    return depth || left.localeCompare(right);
  });
  for (const directory of orderedDirectories) ensureSnapshotDirectory(sessionDir, directory, directoryIdentities);

  const frozenFiles = [];
  for (const file of sourceFiles) {
    const destination = path.join(sessionDir, file.destination);
    const identity = writePrivateFile(destination, file.captured.bytes, 0o400);
    identities.set(destination, identity);
    const copied = readStableRegularFile(destination);
    if (copied.digest !== file.captured.digest || copied.size !== file.captured.size ||
        copied.identity.dev !== identity.dev || copied.identity.ino !== identity.ino) {
      throw new Error(`Frozen runtime copy failed verification: ${file.destination}`);
    }
    frozenFiles.push(Object.freeze({
      path: destination,
      relative: file.destination.split(path.sep).join("/"),
      identity,
      digest: copied.digest,
      size: copied.size,
    }));
  }

  for (const [directory, identity] of directoryIdentities) {
    fs.chmodSync(directory, 0o500);
    const stat = fs.lstatSync(directory);
    if (!sameIdentity(stat, identity) || (stat.mode & 0o222) !== 0) {
      throw new Error(`Frozen runtime directory could not be made read-only: ${directory}`);
    }
  }
  const aggregate = crypto.createHash("sha256");
  for (const file of frozenFiles) aggregate.update(file.relative).update("\0").update(String(file.size)).update("\0").update(file.digest).update("\0");
  const manifest = {
    version: 1,
    aggregate_sha256: aggregate.digest("hex"),
    files: frozenFiles.map((file) => ({ path: file.relative, size: file.size, sha256: file.digest })),
  };
  const manifestFile = path.join(sessionDir, "runtime-manifest.json");
  const manifestIdentity = writePrivateFile(manifestFile, `${JSON.stringify(manifest)}\n`, 0o400);
  identities.set(manifestFile, manifestIdentity);
  const capturedManifest = readStableRegularFile(manifestFile);
  frozenFiles.push(Object.freeze({
    path: manifestFile,
    relative: "runtime-manifest.json",
    identity: manifestIdentity,
    digest: capturedManifest.digest,
    size: capturedManifest.size,
  }));
  const snapshot = Object.freeze({
    brokerPath: path.join(sessionDir, "tools", "claude-codex-broker", "server.mjs"),
    profilePath: path.join(sessionDir, ".claude", "codex-profiles.mjs"),
    hookPath: path.join(sessionDir, ".claude", "hooks", "codex-ceiling-guard.mjs"),
    manifestFile,
    manifest: Object.freeze(manifest),
    files: Object.freeze(frozenFiles),
    directories: Object.freeze([...directoryIdentities].map(([directory, identity]) => Object.freeze({ path: directory, identity }))),
  });
  verifyRuntimeSnapshot(snapshot);
  return snapshot;
}

function usage() {
  return `Usage: scripts/claude-codex.mjs [launcher options] [-- Claude options or prompt]

Launcher options:
  --ceiling repository|isolated-write|system   Static broker ceiling (default: repository)
  --permission-mode MODE                      Mode allowed by the selected ceiling
  --inspect                                   Print the launch plan without starting Claude
  --help                                      Show this help

Defaults:
  repository    plan (manual is also accepted)
  isolated-write acceptEdits (auto or bypassPermissions may be selected)
  system        bypassPermissions only

Examples:
  scripts/claude-codex.mjs
  scripts/claude-codex.mjs --ceiling isolated-write -- --continue
  scripts/claude-codex.mjs --ceiling system -- "finish the production audit"

Security boundary:
  The frozen ceiling and one-use proof fail closed on hook failure, replay, and
  incompatible current permission modes. The grant key is not an OS security
  boundary against a deliberately malicious process running as the same user.
  On Unix, Claude and its MCP descendants run in a dedicated process group that
  the launcher terminates and drains. Windows can only guarantee direct-child
  termination with the Node.js process API used by this launcher.
`;
}

function optionValue(argv, index, name) {
  const current = argv[index];
  if (current === name) {
    const value = argv[index + 1];
    if (!value || value.startsWith("--")) throw new Error(`${name} requires a value.`);
    return { value, consumed: 2 };
  }
  if (current.startsWith(`${name}=`)) {
    const value = current.slice(name.length + 1);
    if (!value) throw new Error(`${name} requires a value.`);
    return { value, consumed: 1 };
  }
  return null;
}

function rejectManagedClaudeFlags(forwarded) {
  for (const argument of forwarded) {
    const forbidden = MANAGED_CLAUDE_FLAGS.find((flag) => argument === flag || argument.startsWith(`${flag}=`));
    if (forbidden) throw new Error(`${forbidden} is controlled by the trusted Codex launcher and cannot be forwarded.`);
  }
}

export function parseLauncherArgs(argv) {
  let ceiling = "repository";
  let requestedMode;
  let inspect = false;
  let help = false;
  let index = 0;
  for (; index < argv.length; ) {
    if (argv[index] === "--") {
      index += 1;
      break;
    }
    if (argv[index] === "--inspect") {
      inspect = true;
      index += 1;
      continue;
    }
    if (argv[index] === "--help" || argv[index] === "-h") {
      help = true;
      index += 1;
      continue;
    }
    const ceilingOption = optionValue(argv, index, "--ceiling");
    if (ceilingOption) {
      ceiling = ceilingOption.value;
      index += ceilingOption.consumed;
      continue;
    }
    const modeOption = optionValue(argv, index, "--permission-mode");
    if (modeOption) {
      requestedMode = modeOption.value;
      index += modeOption.consumed;
      continue;
    }
    throw new Error(`Unknown launcher option ${JSON.stringify(argv[index])}; put Claude arguments after --.`);
  }

  const forwarded = argv.slice(index);
  rejectManagedClaudeFlags(forwarded);
  requireProfile(ceiling);
  const permissionMode = normalizeLauncherMode(ceiling, requestedMode);
  return Object.freeze({ ceiling, permissionMode, inspect, help, forwarded: Object.freeze(forwarded) });
}

export function buildMcpConfig({ projectRoot, ceiling, grantKeyFile, nodePath = process.execPath, brokerPath = BROKER_PATH }) {
  const root = fs.realpathSync(projectRoot);
  requireProfile(ceiling);
  if (!path.isAbsolute(nodePath) || !path.isAbsolute(root) || !path.isAbsolute(brokerPath)) {
    throw new Error("Broker executables and roots must be absolute.");
  }
  const args = [brokerPath, "--repository", root, "--ceiling", ceiling];
  if (ceiling !== "repository") {
    if (!grantKeyFile || !path.isAbsolute(grantKeyFile)) throw new Error("Elevated profiles require an absolute grant key path.");
    args.push("--grant-key-file", grantKeyFile);
  } else if (grantKeyFile != null) {
    throw new Error("The repository profile must not receive an elevated grant key.");
  }
  return {
    mcpServers: {
      [BROKER_SERVER_NAME]: {
        type: "stdio",
        command: nodePath,
        args,
        env: {},
      },
    },
  };
}

export function buildHookSettings({ nodePath = process.execPath, hookPath = HOOK_PATH }) {
  if (!path.isAbsolute(nodePath) || !path.isAbsolute(hookPath)) {
    throw new Error("The elevated hook interpreter and script must use absolute paths.");
  }
  return {
    hooks: {
      PreToolUse: [
        {
          matcher: `^mcp__${BROKER_SERVER_NAME}__.*$`,
          hooks: [
            {
              type: "command",
              command: nodePath,
              args: [hookPath, "--issue-grant"],
              timeout: 5,
            },
          ],
        },
      ],
    },
  };
}

export function buildClaudeArgs({ configFile, settingsFile, permissionMode, forwarded = [] }) {
  if (!path.isAbsolute(configFile) || !path.isAbsolute(settingsFile)) {
    throw new Error("Claude launch configuration paths must be absolute.");
  }
  rejectManagedClaudeFlags(forwarded);
  const args = [
    "--strict-mcp-config",
    "--mcp-config",
    configFile,
    "--setting-sources",
    PRIVATE_SETTING_SOURCES,
    "--settings",
    settingsFile,
    "--permission-mode",
    permissionMode,
  ];
  if (permissionMode === "bypassPermissions") args.push("--dangerously-skip-permissions");
  args.push(...forwarded);
  return args;
}

function sameIdentity(stat, identity) {
  return String(stat.dev) === identity.dev && String(stat.ino) === identity.ino;
}

function identityOf(stat) {
  return Object.freeze({ dev: String(stat.dev), ino: String(stat.ino) });
}

function writePrivateFile(filePath, data, mode = 0o600) {
  let identity = null;
  try {
    fs.writeFileSync(filePath, data, { flag: "wx", mode });
    const created = fs.lstatSync(filePath);
    identity = identityOf(created);
    fs.chmodSync(filePath, mode);
    const stat = fs.lstatSync(filePath);
    if (!sameIdentity(stat, identity) || !stat.isFile() || stat.isSymbolicLink() || (stat.mode & 0o777) !== mode) {
      throw new Error(`Could not create private launcher artifact ${path.basename(filePath)}.`);
    }
    return identity;
  } catch (error) {
    if (identity) {
      try {
        const stat = fs.lstatSync(filePath);
        if (sameIdentity(stat, identity) && stat.isFile() && !stat.isSymbolicLink()) fs.unlinkSync(filePath);
      } catch {}
    }
    throw error;
  }
}

function frozenFileRecord(filePath, identity, expectedMode, label) {
  const captured = readStableRegularFile(filePath);
  if (captured.identity.dev !== identity.dev || captured.identity.ino !== identity.ino || captured.mode !== expectedMode) {
    throw new Error(`${label} failed private-file verification.`);
  }
  return Object.freeze({
    path: filePath,
    identity,
    size: captured.size,
    digest: captured.digest,
    mode: expectedMode,
    label,
  });
}

function verifyFrozenFileRecord(record) {
  const captured = readStableRegularFile(record.path);
  if (captured.identity.dev !== record.identity.dev || captured.identity.ino !== record.identity.ino ||
      captured.size !== record.size || captured.digest !== record.digest || captured.mode !== record.mode) {
    throw new Error(`${record.label} changed after launcher initialization.`);
  }
}

export function createSessionArtifacts({
  projectRoot = PROJECT_ROOT,
  ceiling,
  permissionMode,
  temporaryRoot = os.tmpdir(),
  nodePath = process.execPath,
}) {
  validateProjectSurface(projectRoot);
  const normalizedMode = normalizeLauncherMode(ceiling, permissionMode);
  if (permissionMode !== normalizedMode) throw new Error("The launcher permission mode must be explicit and profile-compatible.");
  const canonicalTemporaryRoot = fs.realpathSync(temporaryRoot);
  const sessionDir = fs.mkdtempSync(path.join(canonicalTemporaryRoot, SESSION_PREFIX));
  const identities = new Map();
  const directoryIdentities = new Map();
  const configFile = path.join(sessionDir, "mcp.json");
  const settingsFile = path.join(sessionDir, "settings.json");
  const grantKeyFile = ceiling === "repository" ? null : path.join(sessionDir, "grant.key");
  let directoryIdentity = null;
  let cleaned = false;
  const controlFiles = [];

  function cleanup() {
    if (cleaned) return [];
    cleaned = true;
    const warnings = [];
    for (const [directory, identity] of directoryIdentities) {
      try {
        const stat = fs.lstatSync(directory);
        if (!sameIdentity(stat, identity) || !stat.isDirectory() || stat.isSymbolicLink()) {
          warnings.push(`Refused to alter replaced launcher directory ${directory}.`);
          continue;
        }
        fs.chmodSync(directory, 0o700);
      } catch (error) {
        if (error?.code !== "ENOENT") warnings.push(`Could not make launcher directory removable ${directory}.`);
      }
    }
    for (const [artifact, identity] of identities) {
      try {
        const stat = fs.lstatSync(artifact);
        if (!sameIdentity(stat, identity) || !stat.isFile() || stat.isSymbolicLink()) {
          warnings.push(`Refused to remove replaced launcher artifact ${artifact}.`);
          continue;
        }
        fs.unlinkSync(artifact);
      } catch (error) {
        if (error?.code !== "ENOENT") warnings.push(`Could not remove launcher artifact ${artifact}.`);
      }
    }
    const directories = [...directoryIdentities].sort((left, right) => right[0].split(path.sep).length - left[0].split(path.sep).length);
    for (const [directory, identity] of directories) {
      try {
        const stat = fs.lstatSync(directory);
        if (!sameIdentity(stat, identity) || !stat.isDirectory() || stat.isSymbolicLink()) {
          warnings.push(`Refused to remove replaced launcher directory ${directory}.`);
          continue;
        }
        fs.rmdirSync(directory);
      } catch (error) {
        if (error?.code !== "ENOENT") warnings.push(`Could not remove launcher directory ${directory}.`);
      }
    }
    try {
      const stat = fs.lstatSync(sessionDir);
      if (!directoryIdentity || !sameIdentity(stat, directoryIdentity) || !stat.isDirectory() || stat.isSymbolicLink()) {
        warnings.push(`Refused to remove replaced launcher directory ${sessionDir}.`);
      } else {
        fs.rmdirSync(sessionDir);
      }
    } catch (error) {
      if (error?.code !== "ENOENT") warnings.push(`Could not remove launcher directory ${sessionDir}.`);
    }
    return warnings;
  }

  try {
    fs.chmodSync(sessionDir, 0o700);
    const directoryStat = fs.lstatSync(sessionDir);
    directoryIdentity = identityOf(directoryStat);
    if (!directoryStat.isDirectory() || directoryStat.isSymbolicLink() || (directoryStat.mode & 0o077) !== 0) {
      throw new Error("Could not create a private Codex launcher session directory.");
    }
    if (grantKeyFile) {
      const identity = writePrivateFile(grantKeyFile, crypto.randomBytes(32), 0o600);
      identities.set(grantKeyFile, identity);
      controlFiles.push(frozenFileRecord(grantKeyFile, identity, 0o600, "The one-use grant key"));
    }
    const runtimeSnapshot = snapshotRuntimeSurface({ sessionDir, identities, directoryIdentities });
    const mcpConfig = buildMcpConfig({
      projectRoot, ceiling, grantKeyFile, nodePath, brokerPath: runtimeSnapshot.brokerPath,
    });
    const hookSettings = buildHookSettings({ nodePath, hookPath: runtimeSnapshot.hookPath });
    const configIdentity = writePrivateFile(configFile, `${JSON.stringify(mcpConfig)}\n`, 0o400);
    identities.set(configFile, configIdentity);
    controlFiles.push(frozenFileRecord(configFile, configIdentity, 0o400, "The private MCP configuration"));
    const settingsIdentity = writePrivateFile(settingsFile, `${JSON.stringify(hookSettings)}\n`, 0o400);
    identities.set(settingsFile, settingsIdentity);
    controlFiles.push(frozenFileRecord(settingsFile, settingsIdentity, 0o400, "The private hook settings"));
    const claudeArgs = buildClaudeArgs({ configFile, settingsFile, permissionMode });
    verifyRuntimeSnapshot(runtimeSnapshot);
    for (const record of controlFiles) verifyFrozenFileRecord(record);

    const verifySnapshot = () => {
      verifyRuntimeSnapshot(runtimeSnapshot);
      for (const record of controlFiles) verifyFrozenFileRecord(record);
      return true;
    };

    return Object.freeze({
      sessionDir,
      configFile,
      settingsFile,
      grantKeyFile,
      mcpConfig,
      hookSettings,
      claudeArgs,
      runtimeSnapshot,
      verifySnapshot,
      cleanup,
    });
  } catch (error) {
    cleanup();
    throw error;
  }
}

function parseVersion(output) {
  const match = /(?:^|\s)(\d+)\.(\d+)\.(\d+)(?:\s|$)/u.exec(output);
  return match ? match.slice(1).map(Number) : null;
}

function versionAtLeast(actual, minimum) {
  for (let index = 0; index < minimum.length; index += 1) {
    if (actual[index] > minimum[index]) return true;
    if (actual[index] < minimum[index]) return false;
  }
  return true;
}

export function verifyClaudeCli(executable = "claude", runner = spawnSync) {
  const versionResult = runner(executable, ["--version"], { encoding: "utf8", shell: false });
  if (versionResult.error || versionResult.status !== 0) throw new Error("Could not execute Claude Code --version.");
  const versionText = `${versionResult.stdout ?? ""}${versionResult.stderr ?? ""}`;
  const version = parseVersion(versionText);
  if (!version || !versionAtLeast(version, MINIMUM_CLAUDE_VERSION)) {
    throw new Error("Claude Code 2.1.214 or newer is required for blocking PreToolUse semantics.");
  }
  const helpResult = runner(executable, ["--help"], { encoding: "utf8", shell: false });
  if (helpResult.error || helpResult.status !== 0) throw new Error("Could not execute Claude Code --help.");
  const help = `${helpResult.stdout ?? ""}${helpResult.stderr ?? ""}`;
  for (const flag of ["--mcp-config", "--strict-mcp-config", "--permission-mode", "--dangerously-skip-permissions", "--setting-sources", "--settings"]) {
    if (!help.includes(flag)) throw new Error(`Installed Claude Code does not advertise required flag ${flag}.`);
  }
  return Object.freeze({ version: version.join("."), executable });
}

export function launcherEnvironment(baseEnvironment, ceiling, grantKeyFile) {
  const environment = { ...baseEnvironment, [CEILING_ENV]: ceiling };
  if (grantKeyFile) environment[GRANT_KEY_FILE_ENV] = grantKeyFile;
  else delete environment[GRANT_KEY_FILE_ENV];
  return environment;
}

const SIGNAL_EXIT_CODES = Object.freeze({ SIGHUP: 129, SIGINT: 130, SIGTERM: 143 });

function signalClaudeTree(child, ownsProcessGroup, signal) {
  if (ownsProcessGroup && Number.isSafeInteger(child.pid) && child.pid > 1) {
    try {
      process.kill(-child.pid, signal);
      return true;
    } catch (error) {
      if (error?.code === "ESRCH" && (child.exitCode !== null || child.signalCode !== null)) return false;
      // Fall through to the direct-child API if this platform unexpectedly
      // refuses process-group signaling after a successful detached spawn.
    }
  }
  try {
    return child.kill(signal);
  } catch {
    return false;
  }
}

function processGroupExists(processGroupId) {
  try {
    process.kill(-processGroupId, 0);
    return true;
  } catch (error) {
    if (error?.code === "ESRCH") return false;
    if (error?.code === "EPERM") return true;
    throw error;
  }
}

async function waitForProcessGroupExit(processGroupId, timeoutMs, pollMs = 20) {
  const deadline = Date.now() + Math.max(0, timeoutMs);
  while (processGroupExists(processGroupId)) {
    if (Date.now() >= deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, Math.min(pollMs, Math.max(1, deadline - Date.now()))));
  }
  return true;
}

export async function runClaude(executable, args, environment, options = {}) {
  if (options.executableIdentity) verifyExecutableIdentity(options.executableIdentity);
  const ownsProcessGroup = process.platform !== "win32";
  const child = spawn(executable, args, {
    cwd: PROJECT_ROOT,
    env: environment,
    stdio: "inherit",
    shell: false,
    detached: ownsProcessGroup,
  });
  const graceMs = options.terminationGraceMs ?? 5_000;
  const forceKillWaitMs = options.forceKillWaitMs ?? 2_000;
  const integrityIntervalMs = options.integrityIntervalMs ?? 250;
  let forwardedSignal = null;
  let integrityError = null;
  let terminating = false;
  let terminationStartedAt = null;
  let killTimer = null;
  let integrityTimer = null;

  const terminate = (signal, reason = null) => {
    if (reason && !integrityError) integrityError = reason;
    if (terminating) {
      signalClaudeTree(child, ownsProcessGroup, "SIGKILL");
      return;
    }
    terminating = true;
    terminationStartedAt = Date.now();
    signalClaudeTree(child, ownsProcessGroup, signal);
    killTimer = setTimeout(() => {
      signalClaudeTree(child, ownsProcessGroup, "SIGKILL");
    }, graceMs);
  };

  const signalHandlers = new Map();
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    const handler = () => {
      if (!forwardedSignal) forwardedSignal = signal;
      terminate(signal);
    };
    signalHandlers.set(signal, handler);
    process.on(signal, handler);
  }
  if (typeof options.integrityCheck === "function") {
    integrityTimer = setInterval(() => {
      try { options.integrityCheck(); }
      catch (error) { terminate("SIGTERM", error); }
    }, integrityIntervalMs);
  }

  try {
    const result = await new Promise((resolve, reject) => {
      let settled = false;
      child.once("error", (error) => {
        if (settled) return;
        settled = true;
        reject(error);
      });
      child.once("close", (code, signal) => {
        if (settled) return;
        settled = true;
        resolve({ code, signal });
      });
    });
    if (ownsProcessGroup && Number.isSafeInteger(child.pid) && child.pid > 1 && processGroupExists(child.pid)) {
      if (!terminating) terminate("SIGTERM");
      const elapsed = terminationStartedAt == null ? 0 : Date.now() - terminationStartedAt;
      const graceful = await waitForProcessGroupExit(child.pid, Math.max(0, graceMs - elapsed));
      if (!graceful) {
        signalClaudeTree(child, ownsProcessGroup, "SIGKILL");
        if (!await waitForProcessGroupExit(child.pid, forceKillWaitMs)) {
          throw new Error("Claude Code's detached process group did not terminate after SIGKILL.");
        }
      }
    }
    return Object.freeze({ ...result, forwardedSignal, integrityError });
  } finally {
    if (killTimer) clearTimeout(killTimer);
    if (integrityTimer) clearInterval(integrityTimer);
    for (const [signal, handler] of signalHandlers) process.removeListener(signal, handler);
  }
}

async function main() {
  let parsed;
  try {
    parsed = parseLauncherArgs(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`${error.message}\n\n${usage()}`);
    process.exitCode = 2;
    return;
  }
  if (parsed.help) {
    process.stdout.write(usage());
    return;
  }

  let claudeExecutable;
  try {
    claudeExecutable = resolveClaudeExecutable(process.env.CLAUDE_CODE_EXECUTABLE, process.env);
    verifyClaudeCli(claudeExecutable.path);
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
    return;
  }

  const artifacts = createSessionArtifacts({
    ceiling: parsed.ceiling,
    permissionMode: parsed.permissionMode,
  });
  const cleanupAtExit = () => { artifacts.cleanup(); };
  process.once("exit", cleanupAtExit);
  try {
    const args = buildClaudeArgs({
      configFile: artifacts.configFile,
      settingsFile: artifacts.settingsFile,
      permissionMode: parsed.permissionMode,
      forwarded: parsed.forwarded,
    });
    const environment = launcherEnvironment(process.env, parsed.ceiling, artifacts.grantKeyFile);
    if (parsed.inspect) {
      process.stdout.write(`${JSON.stringify({
        ceiling: parsed.ceiling,
        permission_mode: parsed.permissionMode,
        cwd: PROJECT_ROOT,
        executable: claudeExecutable.path,
        argv: args,
        mcp_config: artifacts.mcpConfig,
        hook_settings: artifacts.hookSettings,
        grant_key_file: artifacts.grantKeyFile,
        runtime_snapshot: {
          manifest_file: artifacts.runtimeSnapshot.manifestFile,
          aggregate_sha256: artifacts.runtimeSnapshot.manifest.aggregate_sha256,
          files: artifacts.runtimeSnapshot.manifest.files.length,
        },
      }, null, 2)}\n`);
      return;
    }

    const result = await runClaude(claudeExecutable.path, args, environment, {
      integrityCheck: artifacts.verifySnapshot,
      executableIdentity: claudeExecutable,
    });
    if (result.integrityError) {
      process.stderr.write("The frozen Codex broker runtime changed; Claude Code was terminated.\n");
      process.exitCode = 1;
    } else if (result.forwardedSignal) {
      process.exitCode = SIGNAL_EXIT_CODES[result.forwardedSignal] ?? 1;
    } else if (result.signal) {
      process.stderr.write(`Claude Code exited after signal ${result.signal}.\n`);
      process.exitCode = SIGNAL_EXIT_CODES[result.signal] ?? 1;
    } else {
      process.exitCode = result.code ?? 1;
    }
  } finally {
    process.removeListener("exit", cleanupAtExit);
    for (const warning of artifacts.cleanup()) process.stderr.write(`${warning}\n`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main();
