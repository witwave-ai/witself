#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const expectedRateLimits = Object.freeze([
  Object.freeze({
    name: "SUPPORT_EMAIL_SENDER_LIMITER",
    namespace_id: "2401",
    simple: Object.freeze({ limit: 10, period: 60 }),
  }),
  Object.freeze({
    name: "SUPPORT_EMAIL_GLOBAL_LIMITER",
    namespace_id: "2402",
    simple: Object.freeze({ limit: 100, period: 60 }),
  }),
]);

function parseJSONC(raw) {
  return JSON.parse(raw.replace(/^\s*\/\/.*$/gm, ""));
}

export function assertExactConfig(config) {
  const expectedTopLevel = [
    "$schema", "compatibility_date", "compatibility_flags", "main", "name",
    "observability", "preview_urls", "ratelimits", "secrets", "vars",
    "workers_dev",
  ];
  if (!config || typeof config !== "object" || Array.isArray(config) ||
      JSON.stringify(Object.keys(config).sort()) !==
        JSON.stringify(expectedTopLevel) ||
      config.name !== "witself-support-email-intake" ||
      config.main !== "src/index.js" || config.workers_dev !== false ||
      config.preview_urls !== false || config.observability?.enabled !== true ||
      JSON.stringify(config.compatibility_flags) !==
        JSON.stringify(["global_fetch_strictly_public"]) ||
      JSON.stringify(config.ratelimits) !== JSON.stringify(expectedRateLimits) ||
      JSON.stringify(config.secrets) !== JSON.stringify({
        required: ["CONTROL_PLANE_SUPPORT_INTAKE_TOKEN"],
      })) {
    throw new Error("support email intake Worker configuration drifted");
  }
  const expectedVarNames = [
    "CONTROL_PLANE_URL",
    "SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID",
    "SUPPORT_EMAIL_INTAKE_ENABLED",
    "WITSELF_EDGE_RELEASE_COMMIT",
    "WITSELF_EDGE_RELEASE_DATE",
    "WITSELF_EDGE_RELEASE_VERSION",
  ];
  if (JSON.stringify(Object.keys(config.vars ?? {}).sort()) !==
      JSON.stringify(expectedVarNames)) {
    throw new Error("support email intake Worker variable inventory drifted");
  }
  return true;
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: root,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    timeout: 60_000,
    ...options,
  });
  if (result.error || result.status !== 0) {
    const detail = result.error?.message ?? String(result.stderr ?? "")
      .trim().slice(-2_000);
    throw new Error(
      `support email intake Worker bundle check failed${detail ? `: ${detail}` : ""}`,
    );
  }
  return String(result.stdout ?? "");
}

export async function main() {
  const temporary = await mkdtemp(join(tmpdir(), "witself-support-email-intake-"));
  try {
    const configPath = join(temporary, "wrangler.generated.jsonc");
    const output = join(temporary, "bundle");
    const environment = {
      ...process.env,
      SUPPORT_EMAIL_INTAKE_ENABLED: "false",
      SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID: "",
      CONTROL_PLANE_URL: "https://self.witwave.ai/",
      WRANGLER_WRITE_LOGS: "false",
      WRANGLER_LOG_SANITIZE: "true",
      WRANGLER_SEND_METRICS: "false",
      WRANGLER_SEND_ERROR_REPORTS: "false",
      NO_COLOR: "1",
      TERM: "dumb",
    };
    run(process.execPath, [
      join(root, "scripts", "render-wrangler.mjs"),
      "--output", configPath,
    ], { env: environment });
    const config = parseJSONC(await readFile(configPath, "utf8"));
    assertExactConfig(config);
    if (config.vars.SUPPORT_EMAIL_INTAKE_ENABLED !== "false" ||
        config.vars.SUPPORT_EMAIL_AUTH_RESULTS_AUTHSERV_ID !== "") {
      throw new Error("support email intake Worker bundle was not rendered dark");
    }
    run("wrangler", [
      "deploy", join(root, "src", "index.js"), "--dry-run",
      "--config", configPath, "--outdir", output,
    ], { env: environment });
    const files = await readdir(output);
    if (!files.includes("index.js")) {
      throw new Error("support email intake Worker bundle omitted its entrypoint");
    }
    const bundle = await readFile(join(output, "index.js"), "utf8");
    for (const marker of [
      "/v1/intake/support-email",
      "CONTROL_PLANE_SUPPORT_INTAKE_TOKEN",
      "SUPPORT_EMAIL_INTAKE_ENABLED",
      "SUPPORT_EMAIL_SENDER_LIMITER",
      "SUPPORT_EMAIL_GLOBAL_LIMITER",
      "support email intake temporarily unavailable",
    ]) {
      if (!bundle.includes(marker)) {
        throw new Error(`support email intake Worker bundle omitted ${marker}`);
      }
    }
    process.stdout.write("support email intake Worker bundle verified\n");
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
