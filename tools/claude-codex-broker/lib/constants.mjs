export const BROKER_VERSION = "0.1.0";
export const MODEL = "gpt-5.6-sol";
export const EFFORT = "ultra";
export const MULTI_AGENT_VERSION = "v2";
export const REGISTRY = "https://registry.npmjs.org/";
export const MAX_CONCURRENCY = 2;
export const MAX_BROKER_JOBS = 256;
export const MAX_RETAINED_ARTIFACT_BYTES = 256 * 1024 * 1024;
export const MAX_TASK_CHARS = 12_000;
export const MAX_RESULT_BYTES = 128 * 1024;
export const MAX_STATUS_WAIT_SECONDS = 30;
export const JOB_TIMEOUT_MS = 30 * 60 * 1000;

export const PLATFORM_TARGETS = Object.freeze({
  "darwin:arm64": {
    alias: "@openai/codex-darwin-arm64",
    suffix: "darwin-arm64",
    triple: "aarch64-apple-darwin",
  },
  "darwin:x64": {
    alias: "@openai/codex-darwin-x64",
    suffix: "darwin-x64",
    triple: "x86_64-apple-darwin",
  },
  "linux:arm64": {
    alias: "@openai/codex-linux-arm64",
    suffix: "linux-arm64",
    triple: "aarch64-unknown-linux-musl",
  },
  "linux:x64": {
    alias: "@openai/codex-linux-x64",
    suffix: "linux-x64",
    triple: "x86_64-unknown-linux-musl",
  },
  "win32:arm64": {
    alias: "@openai/codex-win32-arm64",
    suffix: "win32-arm64",
    triple: "aarch64-pc-windows-msvc",
  },
  "win32:x64": {
    alias: "@openai/codex-win32-x64",
    suffix: "win32-x64",
    triple: "x86_64-pc-windows-msvc",
  },
});
