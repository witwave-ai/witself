import { constants as fsConstants } from "node:fs";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import process from "node:process";

import {
  EFFORT,
  MODEL,
  PLATFORM_TARGETS,
  REGISTRY,
} from "./constants.mjs";
import {
  BrokerError,
  isContained,
  realpathInside,
  spawnCapture,
} from "./util.mjs";

const VERSION_RE = /^\d+\.\d+\.\d+$/;
const INTEGRITY_RE = /^sha512-[A-Za-z0-9+/]+={0,2}$/;
const KNOWN_PLATFORM_ALIASES = new Set(Object.values(PLATFORM_TARGETS).map(({ alias }) => alias));

function safePath() {
  if (process.platform === "win32") return "C:\\Windows\\System32;C:\\Windows";
  return "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/bin";
}

function privateProcessEnv(overrides = {}) {
  return Object.freeze({
    HOME: overrides.HOME,
    CODEX_HOME: overrides.CODEX_HOME,
    TMPDIR: overrides.TMPDIR,
    PATH: safePath(),
    LANG: "C.UTF-8",
    LC_ALL: "C.UTF-8",
    TERM: "dumb",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_GLOBAL: process.platform === "win32" ? "NUL" : "/dev/null",
    GIT_TERMINAL_PROMPT: "0",
    NO_COLOR: "1",
    ...overrides.extra,
  });
}

async function secureRegularFile(file, { maxBytes = 1024 * 1024, requirePrivate = false, requireCurrentOwner = false } = {}) {
  const info = await fs.lstat(file);
  if (!info.isFile() || info.isSymbolicLink() || info.size > maxBytes) {
    throw new BrokerError("unsafe_file", "A required local credential or runtime file failed validation.");
  }
  if (typeof process.geteuid === "function" && requireCurrentOwner && info.uid !== process.geteuid()) {
    throw new BrokerError("unsafe_file_owner", "A required local credential or runtime file has an unexpected owner.");
  }
  if (requirePrivate && (info.mode & 0o077) !== 0) {
    throw new BrokerError("unsafe_file_mode", "The Codex authentication file must not be accessible to group or other users.");
  }
  return info;
}

async function defaultNpmCommand() {
  const lexical = path.join(path.dirname(process.execPath), process.platform === "win32" ? "npm.cmd" : "npm");
  let npmCli;
  try {
    npmCli = await fs.realpath(lexical);
    await secureRegularFile(npmCli, { maxBytes: 4 * 1024 * 1024 });
  } catch {
    throw new BrokerError("npm_unavailable", "The npm CLI adjacent to the running Node.js executable is unavailable or unsafe.");
  }
  return Object.freeze({ command: process.execPath, prefix: [npmCli] });
}

function parseMetadata(stdout, label) {
  let parsed;
  try {
    parsed = JSON.parse(stdout);
  } catch {
    throw new BrokerError("npm_metadata_invalid", `The official registry returned invalid ${label} metadata.`);
  }
  const version = parsed?.version;
  const integrity = parsed?.["dist.integrity"];
  if (typeof version !== "string" || typeof integrity !== "string" || !INTEGRITY_RE.test(integrity)) {
    throw new BrokerError("npm_metadata_invalid", `The official registry returned incomplete ${label} metadata.`);
  }
  return { version, integrity };
}

function decodeJwtExpiry(token) {
  if (typeof token !== "string" || token.length < 32 || token.length > 64 * 1024) return null;
  const parts = token.split(".");
  if (parts.length !== 3 || parts.some((part) => !/^[A-Za-z0-9_-]+$/.test(part))) return null;
  try {
    const claims = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    return Number.isSafeInteger(claims?.exp) && claims.exp > 0 ? claims.exp : null;
  } catch {
    return null;
  }
}

export async function validateProjectRoot(projectRoot, options = {}) {
  if (typeof projectRoot !== "string" || !path.isAbsolute(projectRoot) || path.normalize(projectRoot) !== projectRoot) {
    throw new BrokerError("invalid_project_root", "The broker requires one canonical absolute project root.");
  }
  let realRoot;
  try {
    const lexical = await fs.lstat(projectRoot);
    if (!lexical.isDirectory() || lexical.isSymbolicLink()) throw new Error("not a canonical directory");
    realRoot = await fs.realpath(projectRoot);
  } catch {
    throw new BrokerError("invalid_project_root", "The configured project root does not resolve to a safe directory.");
  }
  if (realRoot !== projectRoot) {
    throw new BrokerError("invalid_project_root", "The configured project root must already be its canonical real path.");
  }

  const gitCommand = options.gitCommand ?? "/usr/bin/git";
  const runCommand = options.runCommand ?? spawnCapture;
  const env = privateProcessEnv({ HOME: os.homedir(), TMPDIR: os.tmpdir() });
  const top = await runCommand(gitCommand, ["-C", realRoot, "rev-parse", "--show-toplevel"], {
    cwd: realRoot,
    env,
    timeoutMs: 10_000,
    maxStdoutBytes: 16 * 1024,
    maxStderrBytes: 8 * 1024,
  });
  if (top.code !== 0) throw new BrokerError("not_git_root", "The configured project root is not a Git worktree root.");
  const reported = top.stdout.trim();
  let realReported;
  try { realReported = await fs.realpath(reported); } catch { realReported = ""; }
  if (reported !== realRoot || realReported !== realRoot) {
    throw new BrokerError("not_git_root", "The configured project path is not the exact Git worktree root.");
  }
  return realRoot;
}

export class CodexRuntime {
  constructor(options) {
    this.projectRoot = options.projectRoot;
    this.platform = options.platform ?? process.platform;
    this.arch = options.arch ?? process.arch;
    this.target = PLATFORM_TARGETS[`${this.platform}:${this.arch}`];
    this.clock = options.clock ?? Date.now;
    this.tempRoot = options.tempRoot ?? os.tmpdir();
    this.authSource = options.authSource ?? path.join(process.env.CODEX_HOME || path.join(os.homedir(), ".codex"), "auth.json");
    this.npmCommand = options.npmCommand;
    this.runCommand = options.runCommand ?? spawnCapture;
    this.initialPromise = null;
    this.metadata = null;
    this.lastVerifiedAt = null;
    this.restartRequired = false;
    this.sessionDir = null;
    this.binary = null;
    this.codexHome = null;
    this.env = null;
    this.secretValues = [];
    this.gitReadRoots = options.gitReadRoots ?? [];
    this.platformReadRoots = options.platformReadRoots ?? [];
    this.gitCommand = options.gitCommand ?? (process.platform === "win32" ? "git.exe" : "/usr/bin/git");
    this.scratchRoot = null;
    this.deniedSentinel = null;
  }

  static async create(projectRoot, options = {}) {
    const validated = await validateProjectRoot(projectRoot, options);
    const gitCommand = options.gitCommand ?? "/usr/bin/git";
    const runCommand = options.runCommand ?? spawnCapture;
    const gitPaths = await runCommand(gitCommand, ["-C", validated, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir"], {
      cwd: validated,
      env: privateProcessEnv({ HOME: os.homedir(), TMPDIR: os.tmpdir() }),
      timeoutMs: 10_000,
      maxStdoutBytes: 32 * 1024,
      maxStderrBytes: 8 * 1024,
    });
    if (gitPaths.code !== 0) throw new BrokerError("git_metadata_invalid", "The broker could not resolve the Git metadata roots.");
    const lexicalRoots = [...new Set(gitPaths.stdout.trim().split(/\r?\n/).filter(Boolean))];
    if (lexicalRoots.length < 1 || lexicalRoots.length > 2 || lexicalRoots.some((entry) => !path.isAbsolute(entry))) {
      throw new BrokerError("git_metadata_invalid", "Git returned invalid metadata roots.");
    }
    const gitReadRoots = [];
    for (const entry of lexicalRoots) {
      const canonical = await fs.realpath(entry).catch(() => null);
      if (!canonical || canonical !== path.normalize(entry) || !(await fs.stat(canonical)).isDirectory()) {
        throw new BrokerError("git_metadata_invalid", "A Git metadata root was not a canonical directory.");
      }
      gitReadRoots.push(canonical);
    }
    const platformReadRoots = [];
    let confinedGitCommand = gitCommand;
    if (process.platform === "darwin") {
      const developerRoot = "/Library/Developer/CommandLineTools";
      const developerGit = path.join(developerRoot, "usr", "bin", "git");
      const [canonicalRoot, canonicalGit] = await Promise.all([
        fs.realpath(developerRoot).catch(() => null),
        fs.realpath(developerGit).catch(() => null),
      ]);
      if (!canonicalRoot || canonicalRoot !== developerRoot || !canonicalGit || canonicalGit !== developerGit ||
          !(await fs.stat(canonicalRoot)).isDirectory() || !(await fs.stat(canonicalGit)).isFile()) {
        throw new BrokerError("git_toolchain_invalid", "The canonical Apple Git toolchain required by the confinement probe is unavailable.");
      }
      platformReadRoots.push(canonicalRoot);
      confinedGitCommand = canonicalGit;
    }
    return new CodexRuntime({ ...options, projectRoot: validated, gitReadRoots, platformReadRoots, gitCommand: confinedGitCommand });
  }

  async prepareForNewWork() {
    if (this.restartRequired) {
      throw new BrokerError("runtime_restart_required", "The frozen Codex runtime can no longer be verified; restart Claude Code and the broker.");
    }
    if (!this.initialPromise) {
      this.initialPromise = this.#prepareInitial();
      return this.initialPromise;
    }
    await this.initialPromise;
    return this.#reverifyFrozenLatest();
  }

  async #reverifyFrozenLatest() {
    try {
      const current = await this.#resolveLatest();
      if (this.restartRequired) {
        throw new BrokerError("runtime_restart_required", "The frozen Codex runtime can no longer be verified; restart Claude Code and the broker.");
      }
      if (current.version !== this.metadata.version ||
          current.integrity !== this.metadata.integrity ||
          current.platformVersion !== this.metadata.platformVersion ||
          current.platformIntegrity !== this.metadata.platformIntegrity) {
        this.restartRequired = true;
        throw new BrokerError("runtime_update_available", "A newer or changed Codex release is available; restart Claude Code and the broker before starting new work.");
      }
      this.lastVerifiedAt = this.clock();
      return this.describe();
    } catch (error) {
      this.restartRequired = true;
      if (error instanceof BrokerError && error.code === "runtime_update_available") throw error;
      throw new BrokerError("runtime_restart_required", "The frozen Codex runtime could not be reverified; restart Claude Code and the broker.");
    }
  }

  async #prepareInitial() {
    if (!this.target) throw new BrokerError("unsupported_platform", "This operating system and CPU combination is not supported by the constrained Codex broker.");
    this.npmCommand ??= await defaultNpmCommand();
    await this.#createSession();
    const metadata = await this.#resolveLatest();
    await this.#installAndValidate(metadata);
    await this.#prepareCodexHome();
    this.metadata = Object.freeze(metadata);
    return this.#reverifyFrozenLatest();
  }

  async #createSession() {
    const realTemp = await fs.realpath(this.tempRoot);
    const sessionDir = await fs.mkdtemp(path.join(realTemp, "witself-claude-codex-"));
    await fs.chmod(sessionDir, 0o700);
    const info = await fs.stat(sessionDir);
    if (!info.isDirectory() || (info.mode & 0o077) !== 0) {
      throw new BrokerError("unsafe_session", "The private Codex session directory could not be secured.");
    }
    this.sessionDir = sessionDir;
    for (const child of ["npm-home", "npm-cache", "tmp", "codex-home", "jobs", "denied"]) {
      await fs.mkdir(path.join(sessionDir, child), { mode: 0o700 });
      await fs.chmod(path.join(sessionDir, child), 0o700);
    }
    this.scratchRoot = path.join(sessionDir, "jobs");
    this.deniedSentinel = path.join(sessionDir, "denied", "broker-sentinel");
    await fs.writeFile(this.deniedSentinel, `broker-denied-${crypto.randomUUID()}\n`, { mode: 0o600, flag: "wx" });
  }

  #npmEnv() {
    return privateProcessEnv({
      HOME: path.join(this.sessionDir, "npm-home"),
      TMPDIR: path.join(this.sessionDir, "tmp"),
      extra: {
        npm_config_cache: path.join(this.sessionDir, "npm-cache"),
        npm_config_userconfig: process.platform === "win32" ? "NUL" : "/dev/null",
        npm_config_ignore_scripts: "true",
        npm_config_audit: "false",
        npm_config_fund: "false",
        npm_config_update_notifier: "false",
        npm_config_prefer_online: "true",
      },
    });
  }

  async #runNpm(args, timeoutMs = 120_000) {
    const result = await this.runCommand(this.npmCommand.command, [...(this.npmCommand.prefix ?? []), ...args], {
      cwd: this.sessionDir,
      env: this.#npmEnv(),
      timeoutMs,
      maxStdoutBytes: 512 * 1024,
      maxStderrBytes: 128 * 1024,
    });
    if (result.code !== 0) throw new BrokerError("npm_failed", "The official Codex package could not be resolved or installed.");
    return result.stdout;
  }

  async #resolveLatest() {
    const base = parseMetadata(await this.#runNpm([
      "view", "@openai/codex@latest", "version", "dist.integrity", "--json", "--prefer-online", `--registry=${REGISTRY}`,
    ]), "Codex");
    if (!VERSION_RE.test(base.version)) throw new BrokerError("npm_metadata_invalid", "The latest Codex release did not resolve to an exact stable version.");
    const expectedPlatformVersion = `${base.version}-${this.target.suffix}`;
    const platform = parseMetadata(await this.#runNpm([
      "view", `@openai/codex@${expectedPlatformVersion}`, "version", "dist.integrity", "--json", "--prefer-online", `--registry=${REGISTRY}`,
    ]), "Codex platform package");
    if (platform.version !== expectedPlatformVersion) {
      throw new BrokerError("npm_metadata_invalid", "The Codex platform package version did not match the latest release.");
    }
    return {
      version: base.version,
      integrity: base.integrity,
      platformVersion: platform.version,
      platformIntegrity: platform.integrity,
      registry: REGISTRY,
    };
  }

  async #installAndValidate(metadata) {
    const packageJson = {
      name: "witself-private-codex-runtime",
      version: "0.0.0",
      private: true,
      dependencies: { "@openai/codex": metadata.version },
    };
    await fs.writeFile(path.join(this.sessionDir, "package.json"), `${JSON.stringify(packageJson, null, 2)}\n`, { mode: 0o600, flag: "wx" });
    await this.#runNpm([
      "install", "--package-lock-only", "--ignore-scripts", "--no-audit", "--no-fund", "--save-exact",
      "--include=optional", "--prefer-online", `--registry=${REGISTRY}`,
    ]);
    await this.#validateLock(metadata);
    await this.#runNpm([
      "ci", "--ignore-scripts", "--no-audit", "--no-fund", "--include=optional", "--prefer-online", `--registry=${REGISTRY}`,
    ], 180_000);
    await this.#validateInstalled(metadata);
  }

  async #validateLock(metadata) {
    let lock;
    try { lock = JSON.parse(await fs.readFile(path.join(this.sessionDir, "package-lock.json"), "utf8")); }
    catch { throw new BrokerError("package_lock_invalid", "The generated Codex package lock was missing or invalid."); }
    if (lock?.lockfileVersion !== 3 || lock?.packages?.[""]?.dependencies?.["@openai/codex"] !== metadata.version) {
      throw new BrokerError("package_lock_invalid", "The generated Codex package lock did not pin the exact release.");
    }
    const base = lock.packages["node_modules/@openai/codex"];
    const platformKey = `node_modules/${this.target.alias}`;
    const platform = lock.packages[platformKey];
    if (base?.version !== metadata.version || base?.integrity !== metadata.integrity ||
        platform?.version !== metadata.platformVersion || platform?.integrity !== metadata.platformIntegrity) {
      throw new BrokerError("package_lock_invalid", "The Codex package lock integrity did not match official registry metadata.");
    }
    for (const [key, value] of Object.entries(lock.packages)) {
      if (key === "") continue;
      const packageName = key.slice("node_modules/".length);
      if (packageName !== "@openai/codex" && !KNOWN_PLATFORM_ALIASES.has(packageName)) {
        throw new BrokerError("package_lock_invalid", "The Codex package lock contained an unexpected dependency.");
      }
      if (!INTEGRITY_RE.test(value.integrity ?? "") || typeof value.resolved !== "string" || !value.resolved.startsWith(REGISTRY)) {
        throw new BrokerError("package_lock_invalid", "The Codex package lock contained an untrusted source or invalid integrity.");
      }
    }
  }

  async #validateInstalled(metadata) {
    const baseDir = path.join(this.sessionDir, "node_modules", "@openai", "codex");
    const platformDir = path.join(this.sessionDir, "node_modules", ...this.target.alias.split("/"));
    const [basePackage, platformPackage] = await Promise.all([
      fs.readFile(path.join(baseDir, "package.json"), "utf8").then(JSON.parse),
      fs.readFile(path.join(platformDir, "package.json"), "utf8").then(JSON.parse),
    ]).catch(() => { throw new BrokerError("codex_install_invalid", "The installed Codex packages were incomplete."); });
    const expectedAlias = `npm:@openai/codex@${metadata.platformVersion}`;
    if (basePackage.name !== "@openai/codex" || basePackage.version !== metadata.version ||
        basePackage.bin?.codex !== "bin/codex.js" || Object.keys(basePackage.scripts ?? {}).length !== 0 ||
        basePackage.optionalDependencies?.[this.target.alias] !== expectedAlias ||
        platformPackage.name !== "@openai/codex" || platformPackage.version !== metadata.platformVersion ||
        !Array.isArray(platformPackage.os) || !platformPackage.os.includes(this.platform) ||
        !Array.isArray(platformPackage.cpu) || !platformPackage.cpu.includes(this.arch) ||
        Object.keys(platformPackage.scripts ?? {}).length !== 0) {
      throw new BrokerError("codex_install_invalid", "The installed Codex package layout failed compatibility validation.");
    }
    const binaryName = this.platform === "win32" ? "codex.exe" : "codex";
    const binary = path.join(platformDir, "vendor", this.target.triple, "bin", binaryName);
    this.binary = await realpathInside(this.sessionDir, binary);
    const binaryInfo = await secureRegularFile(this.binary, { maxBytes: 512 * 1024 * 1024 });
    if (this.platform !== "win32" && (binaryInfo.mode & 0o111) === 0) {
      throw new BrokerError("codex_install_invalid", "The installed Codex binary was not executable.");
    }
    const version = await this.runCommand(this.binary, ["--version"], {
      cwd: this.projectRoot,
      env: privateProcessEnv({ HOME: path.join(this.sessionDir, "npm-home"), TMPDIR: path.join(this.sessionDir, "tmp") }),
      timeoutMs: 15_000,
      maxStdoutBytes: 4 * 1024,
      maxStderrBytes: 4 * 1024,
    });
    if (version.code !== 0 || version.stdout.trim() !== `codex-cli ${metadata.version}`) {
      throw new BrokerError("codex_version_mismatch", "The installed Codex binary did not attest the resolved exact version.");
    }
  }

  async #prepareCodexHome() {
    this.codexHome = path.join(this.sessionDir, "codex-home");
    await fs.writeFile(path.join(this.codexHome, "config.toml"), "# Intentionally isolated; thread policy is supplied by the broker.\n", { mode: 0o600, flag: "wx" });
    await secureRegularFile(this.authSource, { requirePrivate: true, requireCurrentOwner: true });
    this.env = privateProcessEnv({
      HOME: path.join(this.sessionDir, "npm-home"),
      CODEX_HOME: this.codexHome,
      TMPDIR: path.join(this.sessionDir, "tmp"),
    });
  }

  redact(value) {
    let text = String(value ?? "");
    for (const secret of this.secretValues) text = text.split(secret).join("[REDACTED]");
    return text
      .replace(/\b(?:sk|sess|access|refresh|id)-[A-Za-z0-9._-]{10,}\b/gi, "[REDACTED]")
      .replace(/\bBearer\s+[A-Za-z0-9._~+\/-]{10,}=*\b/gi, "Bearer [REDACTED]");
  }

  describe() {
    if (!this.metadata || !this.binary || !this.env) throw new BrokerError("runtime_not_ready", "The Codex runtime is not ready.");
    return Object.freeze({
      version: this.metadata.version,
      integrity: this.metadata.integrity,
      platformVersion: this.metadata.platformVersion,
      platformIntegrity: this.metadata.platformIntegrity,
      registry: this.metadata.registry,
      latestVerificationPolicy: "before-every-new-work",
      latestVerifiedAt: this.lastVerifiedAt,
      binary: this.binary,
      codexHome: this.codexHome,
      env: this.env,
      projectRoot: this.projectRoot,
      gitReadRoots: Object.freeze([...this.gitReadRoots]),
      platformReadRoots: Object.freeze([...this.platformReadRoots]),
      gitCommand: this.gitCommand,
      scratchRoot: this.scratchRoot,
      deniedSentinel: this.deniedSentinel,
      model: MODEL,
      effort: EFFORT,
      redact: (value) => this.redact(value),
      loadExternalAuth: (minimumTtlMs) => this.loadExternalAuth(minimumTtlMs),
      verifyAuthUnchanged: (expectedHash) => this.verifyAuthUnchanged(expectedHash),
    });
  }

  async #readAuthSource() {
    const noFollow = fsConstants.O_NOFOLLOW ?? 0;
    let handle;
    try {
      handle = await fs.open(this.authSource, fsConstants.O_RDONLY | noFollow);
      const before = await handle.stat();
      if (!before.isFile() || before.size > 1024 * 1024 || (before.mode & 0o077) !== 0 ||
          (typeof process.geteuid === "function" && before.uid !== process.geteuid())) {
        throw new BrokerError("auth_unsafe", "The Codex authentication source failed ownership, mode, type, or size validation.");
      }
      const bytes = await handle.readFile();
      const after = await handle.stat();
      if (before.dev !== after.dev || before.ino !== after.ino || before.size !== after.size || before.mtimeMs !== after.mtimeMs) {
        throw new BrokerError("auth_changed", "The Codex authentication source changed while it was being read; retry with a fresh broker.");
      }
      return { bytes, hash: crypto.createHash("sha256").update(bytes).digest("hex") };
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      throw new BrokerError("auth_unavailable", "A safe current Codex ChatGPT access token is unavailable.");
    } finally {
      await handle?.close().catch(() => {});
    }
  }

  async loadExternalAuth(minimumTtlMs) {
    const { bytes, hash } = await this.#readAuthSource();
    let parsed;
    try { parsed = JSON.parse(bytes.toString("utf8")); }
    catch { throw new BrokerError("auth_invalid", "The local Codex authentication source was invalid JSON."); }
    const accessToken = parsed?.tokens?.access_token;
    const accountId = parsed?.tokens?.account_id;
    const expiresAt = decodeJwtExpiry(accessToken);
    if (!expiresAt || typeof accountId !== "string" || accountId.length < 1 || accountId.length > 512) {
      throw new BrokerError("auth_invalid", "The local Codex authentication source lacks a valid external ChatGPT access token.");
    }
    const remainingMs = expiresAt * 1000 - this.clock();
    if (!Number.isFinite(minimumTtlMs) || minimumTtlMs < 0 || remainingMs <= minimumTtlMs) {
      throw new BrokerError("auth_ttl_insufficient", "The current Codex access token will expire before the bounded job can finish; refresh Codex authentication and restart Claude Code.");
    }
    this.secretValues = [...new Set([...this.secretValues, accessToken, accountId])].sort((a, b) => b.length - a.length).slice(0, 128);
    return Object.freeze({ accessToken, accountId, expiresAt, sourceHash: hash });
  }

  async verifyAuthUnchanged(expectedHash) {
    const { hash } = await this.#readAuthSource();
    if (typeof expectedHash !== "string" || hash !== expectedHash) {
      throw new BrokerError("auth_changed", "The Codex authentication source changed during the delegated operation; no result is trusted.");
    }
    try {
      await fs.lstat(path.join(this.codexHome, "auth.json"));
      throw new BrokerError("auth_persisted", "Codex unexpectedly persisted external authentication in the isolated runtime; no result is trusted.");
    } catch (error) {
      if (error instanceof BrokerError) throw error;
      if (error?.code !== "ENOENT") throw new BrokerError("auth_verification_failed", "The isolated Codex authentication state could not be verified.");
    }
    return true;
  }

  async cleanup() {
    if (!this.sessionDir) return;
    const session = this.sessionDir;
    this.sessionDir = null;
    this.binary = null;
    this.env = null;
    this.secretValues = [];
    this.scratchRoot = null;
    this.deniedSentinel = null;
    const realTemp = await fs.realpath(this.tempRoot).catch(() => null);
    const realSession = await fs.realpath(session).catch(() => null);
    if (!realTemp || !realSession || !isContained(realTemp, realSession) || !path.basename(realSession).startsWith("witself-claude-codex-")) {
      throw new BrokerError("unsafe_cleanup", "The private Codex session directory could not be safely removed.");
    }
    await fs.rm(realSession, { recursive: true, force: false, maxRetries: 2 });
  }

  get version() { return this.metadata?.version; }
  get integrity() { return this.metadata?.integrity; }
  get platformVersion() { return this.metadata?.platformVersion; }
  get platformIntegrity() { return this.metadata?.platformIntegrity; }
  get registry() { return this.metadata?.registry; }
}
