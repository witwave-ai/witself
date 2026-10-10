import { deliveryBounded } from "./entitlement-delivery-metrics.mjs";

export const SIGNUP_OBSERVATION_OBJECT = "signup-observation";
export const SIGNUP_OBSERVATION_KEY = "config:signup_observation";
const STORAGE_KEY = "observation:counts";
// This closed table is the only source of persisted keys, metric names and
// labels. Neither requests nor stored documents contribute label values.
const OBSERVATIONS = [
  ["started", "witself_signup_events_total", '{event="started"}', "Signup lifecycle observations since the singleton was created."],
  ["provisioned", "witself_signup_events_total", '{event="provisioned"}'],
  ["provision_failed", "witself_signup_events_total", '{event="provision_failed"}'],
  ["verified", "witself_signup_events_total", '{event="verified"}'],
  ["quota_per_ip", "witself_signup_denials_total", '{reason="quota_per_ip"}', "Signup denial observations since the singleton was created."],
  ["quota_global", "witself_signup_denials_total", '{reason="quota_global"}'],
  ["turnstile_invalid", "witself_signup_denials_total", '{reason="turnstile_invalid"}'],
  ["turnstile_unavailable", "witself_signup_denials_total", '{reason="turnstile_unavailable"}'],
  ["email_signup_sent", "witself_signup_verification_email_total", '{context="signup",outcome="sent"}', "Verification email send observations since the singleton was created."],
  ["email_signup_failed", "witself_signup_verification_email_total", '{context="signup",outcome="failed"}'],
  ["email_resend_sent", "witself_signup_verification_email_total", '{context="resend",outcome="sent"}'],
  ["email_resend_failed", "witself_signup_verification_email_total", '{context="resend",outcome="failed"}'],
];
const KEYS = OBSERVATIONS.map(([key]) => key);
const fields = (value, keys) => value !== null && typeof value === "object" &&
  !Array.isArray(value) && Object.keys(value).length === keys.length &&
  keys.every((key) => Object.hasOwn(value, key));
const count = (value) => Number.isSafeInteger(value) && value >= 0;
const countsValid = (value) => fields(value, KEYS) && KEYS.every((key) => count(value[key]));
const timestamp = (value) => typeof value === "string" &&
  /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/.test(value) &&
  Number.isFinite(Date.parse(value)) && new Date(value).toISOString() === value;
const pickCounts = (value) => Object.fromEntries(KEYS.map((key) => [key, value[key]]));

export const isSignupObservationKey = (key) => KEYS.includes(key);

export async function signupObservationSnapshot(storage) {
  const stored = await storage.get(STORAGE_KEY);
  if (stored === undefined) return Object.fromEntries(KEYS.map((key) => [key, 0]));
  // Corruption must fail closed, never replace real totals with zeros.
  if (!countsValid(stored)) throw new Error("invalid signup observation counts");
  return pickCounts(stored);
}

export async function recordSignupObservationCount(transaction, key) {
  if (!isSignupObservationKey(key)) throw new Error("unknown signup observation key");
  const counts = await signupObservationSnapshot(transaction);
  if (counts[key] === Number.MAX_SAFE_INTEGER) throw new Error("signup observation count exhausted");
  counts[key] += 1;
  await transaction.put(STORAGE_KEY, counts);
}

export async function recordSignupObservation(env, key, { timeoutMs = 1000 } = {}) {
  try {
    if (!isSignupObservationKey(key) || !env.ACCOUNT_SIGNUP) return;
    const stub = env.ACCOUNT_SIGNUP.get(env.ACCOUNT_SIGNUP.idFromName(SIGNUP_OBSERVATION_OBJECT));
    await deliveryBounded((signal) => stub.fetch(new Request(
      "https://account-signup.internal/observation/record", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ key }), signal,
      },
    )), timeoutMs);
  } catch {
    // Observations must never alter a signup, verify or resend response.
  }
}

const snapshotValid = (value) => fields(value, ["schema_version", "counts"]) &&
  value.schema_version === 1 && countsValid(value.counts);
const rawJSON = (raw) => {
  if (typeof raw !== "string" || raw.length > 4096 || new TextEncoder().encode(raw).length > 4096) return null;
  try { return JSON.parse(raw); } catch { return null; }
};
const documentValid = (value) => {
  if (!fields(value, ["schema_version", "observed_at", "counts", "pending"]) ||
      value.schema_version !== 1 || !countsValid(value.counts) ||
      !timestamp(value.observed_at) || Date.parse(value.observed_at) > Date.now() + 60_000 ||
      !fields(value.pending, ["accounts", "oldest_created_at", "complete"])) return false;
  const pending = value.pending;
  if ((pending.accounts !== null && !count(pending.accounts)) || typeof pending.complete !== "boolean" ||
      (pending.oldest_created_at !== null && (!timestamp(pending.oldest_created_at) ||
        Date.parse(pending.oldest_created_at) > Date.parse(value.observed_at))) ||
      (pending.accounts === null && (pending.oldest_created_at !== null || pending.complete)) ||
      (pending.accounts === 0 && pending.oldest_created_at !== null)) return false;
  return true;
};

async function scanPending(directory, signal) {
  // Stop starting reads before the outer ten-second guard so a settled scan
  // can publish partial coverage without overlapping an outstanding KV call.
  const deadline = Date.now() + 8000;
  const pending = { accounts: null, oldest_created_at: null, complete: false };
  try {
    // One page keeps the KV list cost fixed. An incomplete page remains an
    // explicitly partial scan, even if it contains fewer than 500 keys.
    const page = await directory.list({ prefix: "pending:", limit: 1000 });
    signal.throwIfAborted();
    if (!Array.isArray(page?.keys) || typeof page.list_complete !== "boolean") return pending;
    pending.accounts = 0;
    let processed = 0;
    for (const key of page.keys) {
      if (processed === 500 || Date.now() >= deadline) return pending;
      signal.throwIfAborted();
      const value = await directory.get(key.name, { type: "json" });
      signal.throwIfAborted();
      processed += 1;
      if (value === null) continue;
      pending.accounts += 1;
      const created = typeof value?.created_at === "string" ? Date.parse(value.created_at) : NaN;
      if (Number.isFinite(created)) {
        const iso = new Date(created).toISOString();
        if (pending.oldest_created_at === null || iso < pending.oldest_created_at) pending.oldest_created_at = iso;
      }
    }
    pending.complete = page.list_complete && Date.now() < deadline;
  } catch {
    // A settled failure permits the next sequential write. A deadline abort
    // propagates so a still-running, unabortable KV call cannot overlap it.
    signal.throwIfAborted();
  }
  return pending;
}

export async function runScheduledSignupObservation(env) {
  try {
    if (!env.ACCOUNT_SIGNUP || !env.DIRECTORY) return;
    const stub = env.ACCOUNT_SIGNUP.get(env.ACCOUNT_SIGNUP.idFromName(SIGNUP_OBSERVATION_OBJECT));
    const snapshot = await deliveryBounded(async (signal) => {
      const response = await stub.fetch(new Request("https://account-signup.internal/observation/snapshot", { signal }));
      signal.throwIfAborted();
      if (!response.ok) throw new Error("signup observation snapshot unavailable");
      const value = rawJSON(await response.text());
      signal.throwIfAborted();
      if (!snapshotValid(value)) throw new Error("invalid signup observation snapshot");
      return value;
    }, 5000);
    const pending = await deliveryBounded((signal) => scanPending(env.DIRECTORY, signal), 10_000);
    const document = {
      schema_version: 1, observed_at: new Date().toISOString(),
      counts: pickCounts(snapshot.counts), pending,
    };
    await deliveryBounded(() => env.DIRECTORY.put(SIGNUP_OBSERVATION_KEY, JSON.stringify(document)), 5000);
  } catch {
    // Preserve the last good document on failure; its timestamp ages.
  }
}

export async function signupObservationLines(env) {
  let document = null;
  try {
    document = await deliveryBounded(async (signal) => {
      const raw = await env.DIRECTORY.get(SIGNUP_OBSERVATION_KEY, { type: "text" });
      signal.throwIfAborted();
      const value = rawJSON(raw);
      return documentValid(value) ? value : null;
    }, 5000);
  } catch { /* Independently unavailable; healthy probe scrapes remain HTTP 200. */ }
  const lines = [];
  const gauge = (name, help, value) => lines.push(
    `# HELP ${name} ${help}`, `# TYPE ${name} gauge`, `${name} ${value}`,
  );
  gauge("witself_signup_metrics_up", "Whether a valid signup observation is readable; independent of freshness and scan completeness.", Number(document !== null));
  if (document === null) return lines;
  gauge("witself_signup_observed_timestamp_seconds", "Last successful signup observation publication as Unix seconds.", Date.parse(document.observed_at) / 1000);
  for (const [key, name, labels, help] of OBSERVATIONS) {
    if (help) lines.push(`# HELP ${name} ${help}`, `# TYPE ${name} counter`);
    lines.push(`${name}${labels} ${document.counts[key]}`);
  }
  gauge("witself_signup_pending_scan_complete", "Whether all pending keys were scanned within the read and time limits.", Number(document.pending.complete));
  if (document.pending.accounts !== null) {
    gauge("witself_signup_pending_accounts", "Non-null pending accounts observed in the bounded scan; may be partial.", document.pending.accounts);
    gauge("witself_signup_pending_oldest_age_seconds", "Age of the oldest observed pending creation time; zero means no pending account with a parseable time.",
      document.pending.oldest_created_at === null ? 0 : Math.max(0, (Date.now() - Date.parse(document.pending.oldest_created_at)) / 1000));
  }
  return lines;
}
