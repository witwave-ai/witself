import { spawn } from "node:child_process";

import {
  AgentEmailOperationsLeaseClientError,
} from "./agent-email-operations-lease-client.mjs";

export function runLeaseGuardedCommand(
  command,
  args,
  {
    cwd,
    env,
    signal,
    timeoutMs = 5 * 60_000,
  } = {},
) {
  if (typeof command !== "string" || command === "" ||
      !Array.isArray(args) || args.some((value) => typeof value !== "string") ||
      (env != null && (typeof env !== "object" || Array.isArray(env))) ||
      !Number.isSafeInteger(timeoutMs) || timeoutMs < 1 ||
      timeoutMs > 20 * 60_000) {
    return Promise.reject(new AgentEmailOperationsLeaseClientError(
      "guarded agent email operation command configuration is invalid",
    ));
  }
  return new Promise((resolve, reject) => {
    let child;
    let settled = false;
    let timedOut = false;
    let forceKillTimer = null;
    const finish = (error = null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      clearTimeout(forceKillTimer);
      signal?.removeEventListener("abort", terminate);
      if (error) reject(error);
      else resolve();
    };
    const terminate = () => {
      if (!child || child.exitCode !== null || child.signalCode !== null) return;
      child.kill("SIGTERM");
      forceKillTimer = setTimeout(() => {
        if (child.exitCode === null && child.signalCode === null) {
          child.kill("SIGKILL");
        }
      }, 5_000);
    };
    const timeout = setTimeout(() => {
      timedOut = true;
      terminate();
    }, timeoutMs);
    try {
      child = spawn(command, args, {
        cwd,
        env,
        shell: false,
        stdio: "inherit",
      });
    } catch {
      finish(new AgentEmailOperationsLeaseClientError(
        "guarded agent email operation command could not start",
      ));
      return;
    }
    signal?.addEventListener("abort", terminate, { once: true });
    if (signal?.aborted) terminate();
    child.once("error", () => finish(new AgentEmailOperationsLeaseClientError(
      signal?.aborted
        ? "guarded agent email operation command stopped after lease loss"
        : timedOut
        ? "guarded agent email operation command timed out"
        : "guarded agent email operation command failed",
    )));
    child.once("close", (code) => {
      if (code === 0 && !signal?.aborted && !timedOut) {
        finish();
        return;
      }
      finish(new AgentEmailOperationsLeaseClientError(
        signal?.aborted
          ? "guarded agent email operation command stopped after lease loss"
          : timedOut
          ? "guarded agent email operation command timed out"
          : "guarded agent email operation command failed",
      ));
    });
  });
}
