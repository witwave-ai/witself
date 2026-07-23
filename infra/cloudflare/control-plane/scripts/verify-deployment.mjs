#!/usr/bin/env node
import { readFile } from "node:fs/promises";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

function imageVar(config, name) {
  const match = new RegExp(
    `"${name}"\\s*:\\s*"([^"]+)"`,
  ).exec(config);
  if (!match) throw new Error(`generated config is missing image var ${name}`);
  return match[1];
}

export function expectedBuildMetadata(config) {
  return {
    service: "witself-control-plane",
    version: imageVar(config, "VERSION"),
    commit: imageVar(config, "COMMIT"),
    date: imageVar(config, "DATE"),
  };
}

export function deploymentMatches(actual, expected) {
  return actual?.service === expected.service &&
    actual?.version === expected.version &&
    actual?.commit === expected.commit &&
    actual?.date === expected.date;
}

function parseArgs(argv) {
  const out = {
    config: join(root, "wrangler.generated.jsonc"),
    endpoint: process.env.WITSELF_CONTROL_PLANE ?? "https://self.witwave.ai",
    attempts: 36,
    delayMs: 5000,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const name = argv[i];
    if (!["--config", "--endpoint", "--attempts", "--delay-ms"].includes(name)) {
      throw new Error(`unknown argument ${name}`);
    }
    const value = argv[++i];
    if (!value) throw new Error(`${name} requires a value`);
    switch (name) {
    case "--config":
      out.config = isAbsolute(value) ? value : resolve(root, value);
      break;
    case "--endpoint":
      out.endpoint = value;
      break;
    case "--attempts":
      out.attempts = Number(value);
      break;
    case "--delay-ms":
      out.delayMs = Number(value);
      break;
    }
  }
  if (!Number.isInteger(out.attempts) || out.attempts < 1 || out.attempts > 120) {
    throw new Error("--attempts must be an integer from 1 through 120");
  }
  if (!Number.isInteger(out.delayMs) || out.delayMs < 0 || out.delayMs > 30000) {
    throw new Error("--delay-ms must be an integer from 0 through 30000");
  }
  return out;
}

async function sleep(delayMs) {
  if (delayMs === 0) return;
  await new Promise((resolvePromise) => setTimeout(resolvePromise, delayMs));
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const expected = expectedBuildMetadata(await readFile(args.config, "utf8"));
  const url = `${args.endpoint.replace(/\/+$/, "")}/v1/version`;
  let last = "no response";
  for (let attempt = 1; attempt <= args.attempts; attempt += 1) {
    try {
      const response = await fetch(url, {
        headers: { Accept: "application/json" },
        signal: AbortSignal.timeout(10000),
      });
      if (response.ok) {
        const actual = await response.json();
        if (deploymentMatches(actual, expected)) {
          process.stdout.write(
            `verified ${expected.service} ${expected.version} (${expected.commit})\n`,
          );
          return;
        }
        last = `identity mismatch: ${JSON.stringify(actual)}`;
      } else {
        last = `HTTP ${response.status}`;
      }
    } catch (error) {
      last = error?.name === "TimeoutError" ? "request timed out" : "request failed";
    }
    if (attempt < args.attempts) await sleep(args.delayMs);
  }
  throw new Error(
    `deployment did not report the rendered build identity after ${args.attempts} attempts (${last})`,
  );
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
