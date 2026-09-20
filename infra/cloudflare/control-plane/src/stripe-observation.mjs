// Public projection of the Go container's optional, value-free observation.
// Counters are process-lifetime totals sampled by the existing lifecycle tick.
// Never derive metric names, labels or label values from provider payloads.
const OUTCOMES = ["verified", "rejected", "replayed"];
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const fields = (v, keys) => object(v) && Object.keys(v).length === keys.length &&
  keys.every((key) => Object.hasOwn(v, key));
const count = (v) => Number.isSafeInteger(v) && v >= 0;
const timestamp = (v) => typeof v === "string" &&
  /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(v) &&
  Number.isFinite(Date.parse(v)) &&
  new Date(v).toISOString().slice(0, 19) === v.slice(0, 19);

export function stripeObservation(value, checkpointAt) {
  if (!fields(value, ["schema_version", "webhook_events", "reconciliation_failures",
    "oldest_pending_at", "reconciliation_observed", "reconciliation_complete", "observed_at"]) ||
      value.schema_version !== 1 || !fields(value.webhook_events, OUTCOMES) ||
      !OUTCOMES.every((outcome) => count(value.webhook_events[outcome])) ||
      !count(value.reconciliation_failures) || !timestamp(value.observed_at) ||
      !timestamp(checkpointAt) || Date.parse(value.observed_at) > Date.parse(checkpointAt) + 60_000 ||
      Date.parse(checkpointAt) > Date.now() + 60_000 ||
      typeof value.reconciliation_observed !== "boolean" ||
      typeof value.reconciliation_complete !== "boolean" ||
      (value.oldest_pending_at !== null && (!timestamp(value.oldest_pending_at) ||
        Date.parse(value.oldest_pending_at) > Date.parse(value.observed_at))) ||
      (!value.reconciliation_observed && value.oldest_pending_at !== null) ||
      (value.reconciliation_complete && !value.reconciliation_observed) ||
      (value.reconciliation_observed && value.oldest_pending_at === null &&
        !value.reconciliation_complete)) return null;
  return {
    schema_version: 1,
    webhook_events: Object.fromEntries(OUTCOMES.map((outcome) => [outcome, value.webhook_events[outcome]])),
    reconciliation_failures: value.reconciliation_failures,
    oldest_pending_at: value.oldest_pending_at,
    reconciliation_observed: value.reconciliation_observed,
    reconciliation_complete: value.reconciliation_complete,
    observed_at: value.observed_at,
  };
}

export function stripeObservationLines(value, checkpointAt) {
  const observation = stripeObservation(value, checkpointAt);
  if (!observation) return [];
  const events = "witself_cp_stripe_webhook_events_total";
  const lag = "witself_cp_billing_reconciliation_lag_seconds";
  const failures = "witself_cp_billing_reconciliation_failures_total";
  const lines = [
    `# HELP ${events} Stripe webhook verification attempts and already-processed receipt replays; process-lifetime totals sampled by lifecycle cron.`,
    `# TYPE ${events} counter`,
    ...OUTCOMES.map((outcome) => `${events}{outcome="${outcome}"} ${observation.webhook_events[outcome]}`),
  ];
  if (observation.reconciliation_observed) {
    lines.push(
      `# HELP ${lag} Age of the oldest pending mutation receipt observed by bounded reconciliation, or zero after a complete empty scan; not fleet-wide backlog coverage.`,
      `# TYPE ${lag} gauge`,
      `${lag} ${observation.oldest_pending_at === null ? 0 :
        Math.max(0, (Date.now() - Date.parse(observation.oldest_pending_at)) / 1000)}`,
    );
  }
  lines.push(
    `# HELP ${failures} Process-lifetime account and billing mutation reconciliation failures sampled by lifecycle cron.`,
    `# TYPE ${failures} counter`,
    `${failures} ${observation.reconciliation_failures}`,
  );
  return lines;
}
