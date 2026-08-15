import { readFileSync } from "node:fs";
import { devNull } from "node:os";
import { resolve } from "node:path";

const REVIEWED_ENV_FILE_CONTENT =
  "# Intentionally empty: production Wrangler commands must not load local dotenv files.\n";

export const PRODUCTION_CLOUDFLARE_ACCOUNT_ID =
  "8f0bf04a4e7aab3a8cc60f02cc8c8fdb";

// The default is an operating-system null device rather than a checkout file.
// `withReviewedWranglerEnvironmentFile` validates arguments before spawning
// Wrangler, so a mutable ordinary file would otherwise leave a validation-to-
// open race. The null device cannot acquire dotenv content between those two
// steps. A caller may instead supply a frozen release-snapshot file below.
export const WRANGLER_PRODUCTION_ENV_FILE = devNull;

const WRANGLER_ALLOWED_ENVIRONMENT = Object.freeze([
  // Minimal process/runtime context.
  "PATH",
  "HOME",
  "USER",
  "LOGNAME",
  "SHELL",
  "TMPDIR",
  "TMP",
  "TEMP",
  "LANG",
  "TZ",
  "CI",
  "GITHUB_ACTIONS",
  // One canonical provider identity. Aliases and profiles remain forbidden.
  "CLOUDFLARE_API_TOKEN",
  "CLOUDFLARE_ACCOUNT_ID",
  // Reviewed non-secret inputs consumed by the release config renderers.
  "EMAIL_DIRECTORY_KV_ID",
  "RELAY_KEY_ID",
  "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS",
  "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
  "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
  "AGENT_EMAIL_ROUTE_SIGNING_KEY_ID",
  "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
  "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
]);

// Wrangler accepts several environment variables that can redirect its API,
// authentication, Worker name, logs, or output. Production email operations
// always start from an explicit canonical CLOUDFLARE_* identity and pass only
// a scrubbed child environment to Wrangler.
export const WRANGLER_UNSAFE_ENVIRONMENT = Object.freeze([
  "CF_ACCOUNT_ID",
  "CF_API_TOKEN",
  "CONTROL_PLANE_EDGE_TOKEN",
  "CONTROL_PLANE_URL",
  "CLOUDFLARE_BASE_URL",
  "CLOUDFLARE_API_BASE_URL",
  "CLOUDFLARE_ENV",
  "CF_API_BASE_URL",
  "DOTENV_KEY",
  "WRANGLER_API_ENVIRONMENT",
  "WRANGLER_LOG_PATH",
  "WRANGLER_OUTPUT_FILE_DIRECTORY",
  "WRANGLER_OUTPUT_FILE_PATH",
  "WRANGLER_CI_OVERRIDE_NAME",
  "WRANGLER_AUTH_DOMAIN",
  "WRANGLER_AUTH_URL",
  "WRANGLER_REVOKE_URL",
  "WRANGLER_TOKEN_URL",
  "NODE_OPTIONS",
  "NODE_DEBUG",
  "NODE_V8_COVERAGE",
  "SSLKEYLOGFILE",
  "WITSELF_CONTROL_PLANE",
  "WITSELF_CONTROL_PLANE_ADDR",
  "WITSELF_ENDPOINT",
]);

export function withReviewedWranglerEnvironmentFile(
  args,
  reviewedEnvironmentFile = WRANGLER_PRODUCTION_ENV_FILE,
) {
  if (!Array.isArray(args) || args.some((value) =>
    typeof value !== "string" || value === "--env-file" ||
    value.startsWith("--env-file=")) ||
    typeof reviewedEnvironmentFile !== "string" ||
    (reviewedEnvironmentFile !== devNull &&
      resolve(reviewedEnvironmentFile) !== reviewedEnvironmentFile)) {
    throw new Error("Wrangler arguments contained an unreviewed environment file");
  }
  if (reviewedEnvironmentFile !== devNull) {
    let content;
    try {
      content = readFileSync(reviewedEnvironmentFile, "utf8");
    } catch {
      throw new Error("reviewed empty Wrangler environment file was unavailable");
    }
    if (content !== REVIEWED_ENV_FILE_CONTENT) {
      throw new Error("reviewed empty Wrangler environment file was not empty");
    }
  }
  // Wrangler 4.120 parses this global flag only after the selected command
  // path (for example, `deployments status ... --env-file PATH`).
  return [...args, "--env-file", reviewedEnvironmentFile];
}

// Never let an interactive Wrangler login, profile, deprecated CF_* alias, or
// local dotenv file select the production email account implicitly. Every
// production inspection and mutation must name the reviewed account and carry
// the canonical API-token variable before its child environment is sanitized.
export function assertProductionCloudflareIdentity(source = process.env) {
  if (!source || typeof source !== "object" ||
      source.CLOUDFLARE_ACCOUNT_ID !== PRODUCTION_CLOUDFLARE_ACCOUNT_ID) {
    throw new Error(
      `CLOUDFLARE_ACCOUNT_ID must identify production account ${PRODUCTION_CLOUDFLARE_ACCOUNT_ID}`,
    );
  }
  const token = source.CLOUDFLARE_API_TOKEN;
  if (typeof token !== "string" || token.length < 1 || token.length > 4096 ||
      token !== token.trim() || /[\s\x00-\x1f\x7f]/u.test(token)) {
    throw new Error("CLOUDFLARE_API_TOKEN is missing or invalid");
  }
  return Object.freeze({
    account_id: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    api_token_present: true,
  });
}

export function sanitizedWranglerEnvironment(source = process.env) {
  const output = Object.fromEntries(
    WRANGLER_ALLOWED_ENVIRONMENT
      .filter((name) => Object.hasOwn(source, name))
      .map((name) => [name, source[name]]),
  );
  Object.assign(output, {
    WRANGLER_WRITE_LOGS: "false",
    WRANGLER_LOG_SANITIZE: "true",
    WRANGLER_SEND_METRICS: "false",
    WRANGLER_SEND_ERROR_REPORTS: "false",
    WRANGLER_LOG: "error",
    NO_COLOR: "1",
    TERM: "dumb",
  });
  return output;
}

export function sanitizedWranglerInspectionEnvironment(
  source = process.env,
) {
  const output = sanitizedWranglerEnvironment(source);
  // Wrangler 4.120.0 suppresses JSON stdout from read-only inspection
  // commands when WRANGLER_LOG=error. Keep every redirect, telemetry, and
  // file-logging guard while allowing its normal JSON output.
  delete output.WRANGLER_LOG;
  return output;
}
