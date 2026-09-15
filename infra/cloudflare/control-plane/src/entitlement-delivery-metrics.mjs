// The cursor remains private. Public projection contains only closed outcomes
// and observation times, never account ids, cursors, plans or error details.
export const PLAN_LIFECYCLE_CURSOR_KEY = "config:plan_lifecycle_cursor";
const PREFIX = "witself_entitlement_delivery_";
const OUTCOMES = ["scanned", "seeded", "apply_pending", "failed"];
const MAX_BYTES = 64 * 1024;
const MAX_PAGES = 512;

export const planLifecycleEnabled = (env) =>
  String(env.CP_PLAN_LIFECYCLE_ENABLED ?? "").trim().toLowerCase() === "true";

// KV is not abortable. Callers must check the signal after a late read, and
// must not dispatch further work. Already dispatched writes can finish late.
export async function deliveryBounded(operation, milliseconds) {
  const controller = new AbortController();
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(() => operation(controller.signal)),
      new Promise((_, reject) => {
        timer = setTimeout(() => {
          controller.abort();
          reject(new Error("entitlement observation deadline exceeded"));
        }, milliseconds);
      }),
    ]);
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}

const object = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
const fields = (v, keys) => object(v) && Object.keys(v).length === keys.length && keys.every((k) => Object.hasOwn(v, k));
const cursorOK = (v) => v === null || (typeof v === "string" && v.length > 0 && v.length <= 2048);
const timestamp = (v) => typeof v === "string" && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/.test(v) &&
  Number.isFinite(Date.parse(v)) && new Date(v).toISOString() === v;
const count = (n) => Number.isSafeInteger(n) && n >= 0;
const countsOK = (v, max = Number.MAX_SAFE_INTEGER) => fields(v, OUTCOMES) &&
  OUTCOMES.every((k) => count(v[k]) && v[k] <= v.scanned) && v.scanned <= max;
const pickCounts = (v) => Object.fromEntries(OUTCOMES.map((k) => [k, v[k]]));
const fingerprint = async (cursor) => {
  const bytes = new TextEncoder().encode(JSON.stringify(cursor));
  return [...new Uint8Array(await crypto.subtle.digest("SHA-256", bytes))]
    .map((x) => x.toString(16).padStart(2, "0")).join("");
};

async function monitoringFor(document) {
  if (!object(document) || !cursorOK(document.cursor) || !timestamp(document.updated_at)) return null;
  const m = document.monitoring;
  if (!fields(m, ["schema_version", "last_ack_at", "last_page", "cycle", "last_cycle"]) ||
      m.schema_version !== 1 || m.last_ack_at !== document.updated_at ||
      Date.parse(m.last_ack_at) > Date.now() + 60000 ||
      !countsOK(m.last_page, 100)) return null;
  const last = m.last_cycle;
  if (last !== null && (!fields(last, ["started_at", "completed_at", "pages", "counts"]) ||
      !timestamp(last.started_at) || !timestamp(last.completed_at) ||
      Date.parse(last.started_at) > Date.parse(last.completed_at) ||
      Date.parse(last.completed_at) > Date.parse(m.last_ack_at) ||
      !Number.isSafeInteger(last.pages) || last.pages < 1 || last.pages > MAX_PAGES ||
      !countsOK(last.counts, last.pages * 100))) return null;
  if (last && document.cursor === null && last.completed_at === m.last_ack_at &&
      OUTCOMES.some((k) => last.counts[k] < m.last_page[k])) return null;
  const c = m.cycle;
  if (c !== null && (!fields(c, ["started_at", "pages", "counts", "seen", "resume"]) ||
      document.cursor === null || !timestamp(c.started_at) ||
      Date.parse(c.started_at) > Date.parse(m.last_ack_at) ||
      !Number.isSafeInteger(c.pages) || c.pages < 1 || c.pages >= MAX_PAGES ||
      !countsOK(c.counts, c.pages * 100) || !Array.isArray(c.seen) ||
      c.seen.length !== c.pages || new Set(c.seen).size !== c.seen.length ||
      c.seen.some((x) => typeof x !== "string" || !/^[0-9a-f]{64}$/.test(x)) ||
      c.seen[0] !== await fingerprint(null) ||
      OUTCOMES.some((k) => c.counts[k] < m.last_page[k]) ||
      c.resume !== await fingerprint(document.cursor) || c.seen.includes(c.resume))) return null;
  return m;
}

// One optional field is committed with the existing cursor. An invalid or
// missing mid-cycle field cannot promote a tail scan to complete coverage.
async function buildCheckpoint(stored, cursor, next, result, base) {
  if (!cursorOK(next) || !countsOK(pickCounts(result), 100)) throw new Error("invalid observation");
  const previous = await monitoringFor(stored);
  let cycle = cursor == null
    ? { started_at: base.updated_at, pages: 0, counts: { scanned: 0, seeded: 0, apply_pending: 0, failed: 0 }, seen: [] }
    : previous?.cycle;
  let last = previous?.last_cycle ?? null;
  // Exact continuation binding is required even if a caller supplies a
  // valid document alongside a different directory cursor.
  if (cursor != null && stored?.cursor !== cursor) cycle = null;
  if (cycle && Date.parse(cycle.started_at) <= Date.parse(base.updated_at)) {
    const seen = [...cycle.seen, await fingerprint(cursor ?? null)];
    const resume = await fingerprint(next);
    const counts = Object.fromEntries(OUTCOMES.map((k) => [k, cycle.counts[k] + result[k]]));
    const pages = cycle.pages + 1;
    if (pages > MAX_PAGES || !countsOK(counts, pages * 100) || new Set(seen).size !== seen.length ||
        (next !== null && (pages === MAX_PAGES || seen.includes(resume)))) cycle = null;
    else if (next === null) {
      last = { started_at: cycle.started_at, completed_at: base.updated_at, pages, counts };
      cycle = null;
    } else cycle = { started_at: cycle.started_at, pages, counts, seen, resume };
  } else cycle = null;
  const document = { ...base, monitoring: {
    schema_version: 1, last_ack_at: base.updated_at, last_page: pickCounts(result), cycle, last_cycle: last,
  } };
  if (new TextEncoder().encode(JSON.stringify(document)).length > MAX_BYTES || !await monitoringFor(document)) return base;
  return document;
}

export async function writeDeliveryCheckpoint(directory, stored, cursor, next, result) {
  const base = { cursor: next, updated_at: new Date().toISOString() };
  let document = base;
  try {
    // Construction is pure: a late digest result cannot perform a write or
    // replace the outer fallback envelope after this independent deadline.
    document = await deliveryBounded(() => buildCheckpoint(stored, cursor, next, result, base), 1000);
  } catch {
    // Monitoring cannot prevent the pre-existing cursor update.
    // Keep the original envelope.
  }
  try {
    await deliveryBounded(() => directory.put(PLAN_LIFECYCLE_CURSOR_KEY, JSON.stringify(document)), 5000);
  } catch (error) {
    if (document === base) throw error;
    // Same exact progress, at most once, without optional metadata. If this
    // also fails, preserve the existing unsuccessful scheduled-tick outcome.
    await deliveryBounded(() => directory.put(PLAN_LIFECYCLE_CURSOR_KEY, JSON.stringify(base)), 5000);
  }
}

export async function entitlementDeliveryLines(env) {
  const enabled = planLifecycleEnabled(env);
  let state = enabled ? "missing" : "disabled", m = null, document;
  if (enabled) {
    try {
      await deliveryBounded(async (signal) => {
        const raw = await env.DIRECTORY.get(PLAN_LIFECYCLE_CURSOR_KEY, { type: "text" });
        signal.throwIfAborted();
        if (raw === null) return;
        state = "invalid";
        if (typeof raw !== "string" || raw.length > MAX_BYTES || new TextEncoder().encode(raw).length > MAX_BYTES) return;
        try { document = JSON.parse(raw); } catch { return; }
        m = await monitoringFor(document);
        signal.throwIfAborted();
        if (m) state = "valid";
      }, 5000);
    } catch { state = "unavailable"; m = null; }
  }
  const lines = [];
  const gauge = (name, help, value, labels = "") => {
    lines.push(`# HELP ${PREFIX}${name} ${help}`, `# TYPE ${PREFIX}${name} gauge`, `${PREFIX}${name}${labels} ${value}`);
  };
  gauge("enabled", "Whether the current Worker lifecycle scheduler is enabled.", Number(enabled));
  gauge("metrics_up", "Whether a valid entitlement checkpoint is readable; does not imply fresh or complete coverage.", Number(state === "valid"));
  gauge("snapshot_state", "Current entitlement observation state.", 1, `{state="${state}"}`);
  if (state !== "valid") return lines;
  gauge("last_ack_timestamp_seconds", "Last fully accounted CP page acknowledgement, including account failures, as Unix seconds.", Date.parse(m.last_ack_at) / 1000);
  gauge("cycle_in_progress", "Whether the private scan cursor has a continuation; coverage may be unknown.", Number(document.cursor !== null));
  gauge("cycle_started_timestamp_seconds", "Start of the current measured traversal, or zero when none is measured.", m.cycle ? Date.parse(m.cycle.started_at) / 1000 : 0);
  gauge("cycle_coverage_complete", "Whether a previous complete measured traversal exists; not instantaneous fleet coverage.", Number(m.last_cycle !== null));
  gauge("last_cycle_completed_timestamp_seconds", "Last complete measured traversal as Unix seconds, or zero before any complete traversal.", m.last_cycle ? Date.parse(m.last_cycle.completed_at) / 1000 : 0);
  for (const [name, values] of [["last_page_accounts", m.last_page], ["last_cycle_accounts", m.last_cycle?.counts]]) {
    if (!values) continue;
    lines.push(`# HELP ${PREFIX}${name} Account observations over the last acknowledged page or complete traversal; pending and failed overlap and must not be summed.`, `# TYPE ${PREFIX}${name} gauge`);
    for (const outcome of OUTCOMES) lines.push(`${PREFIX}${name}{outcome="${outcome}"} ${values[outcome]}`);
  }
  return lines;
}
