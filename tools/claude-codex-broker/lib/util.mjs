import { spawn } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";

export class BrokerError extends Error {
  constructor(code, publicMessage) {
    super(publicMessage);
    this.name = "BrokerError";
    this.code = code;
    this.publicMessage = publicMessage;
  }
}

export function boundedText(value, max = 2_000) {
  const text = String(value ?? "").replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g, "");
  return text.length <= max ? text : `${text.slice(0, max - 1)}…`;
}

export function newId() {
  return crypto.randomUUID();
}

export function isContained(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === "" || (!relative.startsWith(`..${path.sep}`) && relative !== ".." && !path.isAbsolute(relative));
}

export async function realpathInside(root, candidate) {
  const [realRoot, realCandidate] = await Promise.all([fs.realpath(root), fs.realpath(candidate)]);
  if (!isContained(realRoot, realCandidate)) {
    throw new BrokerError("unsafe_path", "A runtime path escaped its private session directory.");
  }
  return realCandidate;
}

export function killProcessTree(child, signal = "SIGTERM") {
  if (!child) return;
  try {
    if (process.platform !== "win32" && child.pid) process.kill(-child.pid, signal);
    else if (child.exitCode === null && child.signalCode === null) child.kill(signal);
  } catch {
    // The child may have exited between the state check and signal.
  }
}

export function spawnCapture(command, args, options = {}) {
  const {
    cwd,
    env,
    timeoutMs = 30_000,
    maxStdoutBytes = 256 * 1024,
    maxStderrBytes = 64 * 1024,
    input,
  } = options;

  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      env,
      detached: process.platform !== "win32",
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    });
    let stdout = Buffer.alloc(0);
    let stderr = Buffer.alloc(0);
    let exceeded = false;
    let timedOut = false;
    let settled = false;
    let forceKillTimer;

    const append = (current, chunk, limit) => {
      if (current.length + chunk.length > limit) {
        exceeded = true;
        killProcessTree(child);
        forceKillTimer ??= setTimeout(() => killProcessTree(child, "SIGKILL"), 1_000);
        forceKillTimer.unref?.();
        return current;
      }
      return Buffer.concat([current, chunk]);
    };
    child.stdout.on("data", (chunk) => { stdout = append(stdout, chunk, maxStdoutBytes); });
    child.stderr.on("data", (chunk) => { stderr = append(stderr, chunk, maxStderrBytes); });
    child.on("error", () => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(forceKillTimer);
      reject(new BrokerError("process_start_failed", "A required local process could not be started."));
    });
    child.on("close", (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      clearTimeout(forceKillTimer);
      if (exceeded) return reject(new BrokerError("process_output_limit", "A required local process exceeded its output limit."));
      if (timedOut) return reject(new BrokerError("process_timeout", "A required local process timed out."));
      resolve({ code, signal, stdout: stdout.toString("utf8"), stderr: stderr.toString("utf8") });
    });
    const timer = setTimeout(() => {
      timedOut = true;
      killProcessTree(child);
      forceKillTimer ??= setTimeout(() => killProcessTree(child, "SIGKILL"), 1_000);
      forceKillTimer.unref?.();
    }, timeoutMs);
    timer.unref?.();
    if (input !== undefined) child.stdin.end(input);
    else child.stdin.end();
  });
}

export function publicError(error) {
  if (error instanceof BrokerError) return { code: error.code, message: error.publicMessage };
  return { code: "internal_error", message: "The broker encountered an internal error." };
}
