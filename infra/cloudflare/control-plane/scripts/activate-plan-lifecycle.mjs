#!/usr/bin/env node
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ACTIVATE_PATH = "/v1/internal/plan-lifecycle:activate";
const ACTIVATE_TIMEOUT_MS = 3 * 60 * 1000;

export function activationConfig(env = process.env) {
  const token = String(env.INTERNAL_BRIDGE_TOKEN ?? "").trim();
  if (!token) {
    throw new Error("INTERNAL_BRIDGE_TOKEN must be set in the environment");
  }

  const rawEndpoint = String(
    env.WITSELF_CONTROL_PLANE ?? "https://self.witwave.ai",
  ).trim();
  let endpoint;
  try {
    endpoint = new URL(rawEndpoint);
  } catch {
    throw new Error("WITSELF_CONTROL_PLANE must be a valid HTTPS URL");
  }
  if (endpoint.protocol !== "https:" ||
      endpoint.username || endpoint.password ||
      endpoint.search || endpoint.hash) {
    throw new Error("WITSELF_CONTROL_PLANE must be a valid HTTPS URL");
  }
  endpoint.pathname = `${endpoint.pathname.replace(/\/+$/, "")}${ACTIVATE_PATH}`;

  return { endpoint: endpoint.toString(), token };
}

export async function activatePlanLifecycle(
  { endpoint, token },
  fetchImpl = fetch,
) {
  let response;
  try {
    response = await fetchImpl(endpoint, {
      method: "POST",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${token}`,
      },
      signal: AbortSignal.timeout(ACTIVATE_TIMEOUT_MS),
    });
  } catch {
    throw new Error("plan lifecycle activation request failed");
  }
  if (!response.ok) {
    throw new Error(`plan lifecycle activation failed (HTTP ${response.status})`);
  }

  let doc;
  try {
    doc = await response.json();
  } catch {
    throw new Error("plan lifecycle activation returned an invalid response");
  }
  if (doc?.schema_version !== "witself.v0" ||
      doc?.plan_lifecycle?.activated !== true ||
      doc?.plan_lifecycle?.enabled !== true) {
    throw new Error("plan lifecycle activation was not verified");
  }
}

async function main() {
  await activatePlanLifecycle(activationConfig());
  process.stdout.write("plan lifecycle activated and verified\n");
}

if (process.argv[1] != null &&
    resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
