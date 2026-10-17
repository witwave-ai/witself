// Public projection of the Go container's optional, value-free observation.
// Counters are process-lifetime totals sampled by the existing lifecycle tick.
// Never derive metric names, labels or label values from provider payloads.
const OUTCOMES = ["verified", "rejected", "replayed"];
const V2_OUTCOMES = [...OUTCOMES, "processing_failed"];
const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const fields = (v, keys) => object(v) && Object.keys(v).length === keys.length &&
  keys.every((key) => Object.hasOwn(v, key));
const count = (v) => Number.isSafeInteger(v) && v >= 0;
const timestamp = (v) => typeof v === "string" &&
  /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z$/.test(v) &&
  Number.isFinite(Date.parse(v)) &&
  new Date(v).toISOString().slice(0, 19) === v.slice(0, 19);

export function stripeObservation(value, checkpointAt) {
  const schema2 = value?.schema_version === 2;
  const outcomes = schema2 ? V2_OUTCOMES : OUTCOMES;
  if (!fields(value, ["schema_version", "webhook_events", "reconciliation_failures",
    "oldest_pending_at", "reconciliation_observed", "reconciliation_complete", "observed_at",
    ...(schema2 ? ["mutation_batch_failures", "entitlement_apply_blocked"] : [])]) ||
      (value.schema_version !== 1 && !schema2) || !fields(value.webhook_events, outcomes) ||
      !outcomes.every((outcome) => count(value.webhook_events[outcome])) ||
      (schema2 && (!count(value.mutation_batch_failures) || !count(value.entitlement_apply_blocked) ||
        value.webhook_events.processing_failed > value.webhook_events.verified)) ||
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
  const observation = {
    schema_version: value.schema_version,
    webhook_events: Object.fromEntries(outcomes.map((outcome) => [outcome, value.webhook_events[outcome]])),
    reconciliation_failures: value.reconciliation_failures,
    oldest_pending_at: value.oldest_pending_at,
    reconciliation_observed: value.reconciliation_observed,
    reconciliation_complete: value.reconciliation_complete,
    observed_at: value.observed_at,
  };
  if (schema2) {
    observation.mutation_batch_failures = value.mutation_batch_failures;
    observation.entitlement_apply_blocked = value.entitlement_apply_blocked;
  }
  return observation;
}

export function stripeObservationLines(value, checkpointAt) {
  const observation = stripeObservation(value, checkpointAt);
  if (!observation) return [];
  const events = "witself_cp_stripe_webhook_events_total";
  const lag = "witself_cp_billing_reconciliation_lag_seconds";
  const failures = "witself_cp_billing_reconciliation_failures_total";
  const schema2 = observation.schema_version === 2;
  const eventsHelp = schema2 ?
    "Stripe webhook verification attempts, already-processed receipt replays and verified deliveries whose processing failed; process-lifetime totals sampled by lifecycle cron." :
    "Stripe webhook verification attempts and already-processed receipt replays; process-lifetime totals sampled by lifecycle cron.";
  const lines = [
    `# HELP ${events} ${eventsHelp}`,
    `# TYPE ${events} counter`,
    ...(schema2 ? V2_OUTCOMES : OUTCOMES).map((outcome) => `${events}{outcome="${outcome}"} ${observation.webhook_events[outcome]}`),
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
  if (schema2) {
    const mutationFailures = "witself_cp_billing_mutation_batch_failures_total";
    const blocked = "witself_cp_entitlement_apply_blocked_total";
    lines.push(
      `# HELP ${mutationFailures} Process-lifetime billing-mutation recovery batches that returned an error (list failure, item failure or deadline); sampled by lifecycle cron.`,
      `# TYPE ${mutationFailures} counter`,
      `${mutationFailures} ${observation.mutation_batch_failures}`,
      `# HELP ${blocked} Process-lifetime account reconciliations whose snapshot the cell's atomic fit check refused; pending, not failed; sampled by lifecycle cron.`,
      `# TYPE ${blocked} counter`,
      `${blocked} ${observation.entitlement_apply_blocked}`,
    );
  }
  return lines;
}
