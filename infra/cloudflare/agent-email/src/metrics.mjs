export const EDGE_METRICS_SCHEMA = "witself.agent-email.edge.v1";
export const ROUTE_LOOKUP_METRICS_SCHEMA = "witself.agent-email.route-lookup.v1";

const OUTCOMES = new Set([
  "accepted",
  "discarded_feature_disabled",
  "rejected_cell_capacity",
  "rejected_invalid_recipient",
  "rejected_unknown_recipient",
  "rejected_inactive_route",
  "rejected_over_size",
  "rejected_cell_permanent",
  "rejected_retry_canary",
  "tempfail_configuration",
  "tempfail_disabled",
  "tempfail_directory",
  "tempfail_alias_gate",
  "tempfail_account_cohort",
  "tempfail_canonical_gate",
  "tempfail_custom_domain_gate",
  "tempfail_suspended_route",
  "tempfail_route_lookup",
  "tempfail_content",
  "tempfail_signing",
  "tempfail_transport",
  "tempfail_rate_limited",
  "tempfail_cell_response",
  "tempfail_internal",
]);

const PHASES = new Set([
  "configuration", "recipient", "directory", "content", "signing",
  "fetch", "response", "route", "internal",
]);

const ROUTE_LOOKUP_RESULTS = new Set([
  "kv_fresh",
  "legacy",
  "cp_found",
  "cp_not_found",
  "miss_suppressed",
  "cold_limited",
  "known_limited",
  "kv_error",
  "cp_error",
]);

const ROUTE_LOOKUP_EVIDENCE = new Set(["none", "known", "uncertain"]);
const ROUTE_LOOKUP_KINDS = new Set([
  "canonical", "alias", "custom_domain", "pilot", "unknown",
]);

const RELEASE_VERSION = /^(?:(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)|development-[0-9a-f]{12}(?:-dirty)?)$/;
const RELEASE_COMMIT = /^[0-9a-f]{40}$/;

function validReleaseDate(value) {
  return typeof value === "string" && value.length <= 64 &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value) &&
    !Number.isNaN(Date.parse(value));
}

// Release attribution is deployment metadata, not a sampling dimension. Keep
// it out of indexes and only append the complete renderer-issued triple. A
// missing or malformed binding therefore cannot leak an arbitrary runtime
// value into Analytics Engine or create a partly attributable point.
function releaseAttribution(env) {
  const version = String(env?.WITSELF_EDGE_RELEASE_VERSION ?? "");
  const commit = String(env?.WITSELF_EDGE_RELEASE_COMMIT ?? "");
  const date = String(env?.WITSELF_EDGE_RELEASE_DATE ?? "");
  if (!RELEASE_VERSION.test(version) || !RELEASE_COMMIT.test(commit) ||
      !validReleaseDate(date)) {
    return [];
  }
  return [version, commit, date];
}

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
        ...releaseAttribution(env),
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

// Route lookup telemetry deliberately shares the edge Analytics Engine
// dataset under a distinct schema. It contains only fixed enums and bounded
// numbers: never an address, domain, realm label, route digest, account, or
// tenant identifier. The separate schema keeps existing verdict queries
// stable while making cold-miss pressure and dependency failures visible.
export function recordRouteLookup(env, fields) {
  const dataset = env?.EMAIL_EDGE_METRICS;
  if (!dataset || typeof dataset.writeDataPoint !== "function") return;
  const requestedResult = String(fields?.result ?? "");
  const result = ROUTE_LOOKUP_RESULTS.has(requestedResult) ? requestedResult : "cp_error";
  const requestedEvidence = String(fields?.evidence ?? "");
  const evidence = ROUTE_LOOKUP_EVIDENCE.has(requestedEvidence)
    ? requestedEvidence
    : "uncertain";
  const requestedKind = String(fields?.routeKind ?? "");
  const routeKind = ROUTE_LOOKUP_KINDS.has(requestedKind) ? requestedKind : "unknown";
  try {
    dataset.writeDataPoint({
      indexes: [result],
      blobs: [
        ROUTE_LOOKUP_METRICS_SCHEMA,
        result,
        evidence,
        routeKind,
        ...releaseAttribution(env),
      ],
      doubles: [
        1,
        boundedNonNegative(fields?.durationMS),
        boundedNonNegative(fields?.status),
      ],
    });
  } catch {
    // Metrics are not part of the SMTP transaction contract.
  }
}
