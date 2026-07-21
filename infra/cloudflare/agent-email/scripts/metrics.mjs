#!/usr/bin/env node
import { CloudflareAPI, cloudflareEnvironment } from "./cloudflare.mjs";
import { EDGE_METRICS_SCHEMA } from "../src/metrics.mjs";

export const EDGE_METRICS_DATASET = "witself_agent_email_edge";

export function summaryQuery(minutes) {
  if (!Number.isSafeInteger(minutes) || minutes < 1 || minutes > 10_080) {
    throw new Error("minutes must be an integer from 1 through 10080");
  }
  return `SELECT
  blob2 AS outcome,
  blob3 AS phase,
  SUM(_sample_interval * double1) AS events,
  SUM(_sample_interval * double2) / SUM(_sample_interval * double1) AS average_latency_ms,
  MAX(double2) AS maximum_latency_ms,
  SUM(_sample_interval * double3) AS raw_bytes
FROM ${EDGE_METRICS_DATASET}
WHERE timestamp >= NOW() - INTERVAL '${minutes}' MINUTE
  AND blob1 = '${EDGE_METRICS_SCHEMA}'
GROUP BY outcome, phase
ORDER BY outcome, phase
FORMAT JSON`;
}

export async function runMetrics(argv = process.argv.slice(2), env = process.env, fetchAPI = fetch) {
  const [command = "summary", rawMinutes = "60", ...rest] = argv;
  if (command !== "summary" || rest.length !== 0 || !/^\d+$/.test(rawMinutes)) {
    throw new Error("usage: npm run metrics -- summary [minutes]");
  }
  const minutes = Number(rawMinutes);
  const config = cloudflareEnvironment(env);
  const api = new CloudflareAPI({ ...config, fetchAPI });
  const result = await api.queryAnalytics(summaryQuery(minutes));
  return { schema: "witself.agent-email.edge-summary.v1", window_minutes: minutes, result };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  runMetrics()
    .then((result) => process.stdout.write(`${JSON.stringify(result, null, 2)}\n`))
    .catch((error) => {
      process.stderr.write(`agent-email metrics: ${error.message}\n`);
      process.exitCode = 1;
    });
}
