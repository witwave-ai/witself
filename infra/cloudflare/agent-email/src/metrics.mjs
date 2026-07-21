export const EDGE_METRICS_SCHEMA = "witself.agent-email.edge.v1";

const OUTCOMES = new Set([
  "accepted",
  "rejected_invalid_recipient",
  "rejected_unknown_recipient",
  "rejected_over_size",
  "rejected_cell_permanent",
  "rejected_retry_canary",
  "tempfail_configuration",
  "tempfail_disabled",
  "tempfail_directory",
  "tempfail_content",
  "tempfail_signing",
  "tempfail_transport",
  "tempfail_cell_response",
  "tempfail_internal",
]);

const PHASES = new Set([
  "configuration", "recipient", "directory", "content", "signing",
  "fetch", "response", "internal",
]);

function boundedNonNegative(value) {
  const number = Number(value);
  if (!Number.isFinite(number) || number < 0) return 0;
  return Math.min(number, Number.MAX_SAFE_INTEGER);
}

// recordEdgeVerdict emits one value-free event for every final SMTP-facing
// outcome. Analytics Engine writes are deliberately best-effort and
// non-blocking: an observability outage must never alter mail disposition.
// The projection carries no address, realm, agent, subject, digest, signature,
// message id, or content-derived value.
export function recordEdgeVerdict(env, fields) {
  const dataset = env?.EMAIL_EDGE_METRICS;
  if (!dataset || typeof dataset.writeDataPoint !== "function") return;
  const requestedOutcome = String(fields?.outcome ?? "");
  const outcome = OUTCOMES.has(requestedOutcome) ? requestedOutcome : "tempfail_internal";
  const requestedPhase = String(fields?.phase ?? "");
  const phase = PHASES.has(requestedPhase) ? requestedPhase : "internal";
  try {
    dataset.writeDataPoint({
      // Analytics Engine samples equitably by index. Indexing on this fixed,
      // low-cardinality verdict enum preserves rare rejects and tempfails
      // instead of letting accepted traffic crowd them out.
      indexes: [outcome],
      blobs: [
        EDGE_METRICS_SCHEMA,
        outcome,
        phase,
      ],
      doubles: [
        1,
        boundedNonNegative(fields?.durationMS),
        boundedNonNegative(fields?.rawSize),
        boundedNonNegative(fields?.status),
      ],
    });
  } catch {
    // Metrics are not part of the SMTP transaction contract.
  }
}
