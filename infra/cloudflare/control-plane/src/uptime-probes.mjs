// Use a dedicated five-minute Workers cron and reuse DIRECTORY. An Actions
// schedule would consume about 8,600 runner minutes/month; cron is included in
// the Workers plan already required by this CP (~17k monthly invocations is
// noise against included requests). Alertmanager keeps the PagerDuty secret.
export const UPTIME_PROBES_KEY = "config:uptime_probes";
export const UPTIME_PROBE_SCHEDULER_KEY = "config:uptime_probe_scheduler";
export const UPTIME_PROBE_RUN_KEY = "config:uptime_probe_run";
export const UPTIME_PROBES_PATH = "/metrics/probes";
const CONTROL_PLANE_TARGET = "control_plane"; // Cannot collide with cell names.
const CONTROL_PLANE_URL = "https://self.witwave.ai/v1/version";
const CELL_NAME = /^[a-z0-9-]{1,64}$/;
const TOTAL_BUDGET_MS = 25_000;
const DIRECTORY_TIMEOUT_MS = 10_000;
const SNAPSHOT_TIMEOUT_MS = 5_000;
const PROBE_TIMEOUT_MS = 10_000;
const WRITE_TIMEOUT_MS = 5_000;
// Probes have their own invocation: four fetches leave two of its six connection
// slots for KV work without competing with the separate maintenance trigger.
const PROBE_CONCURRENCY = 4;

// KV calls cannot be aborted. Race them against a deadline and check the signal
// between reads, so a late directory result cannot start another page or reach
// persistence. An already-started KV write may finish after its local deadline.
async function bounded(operation, timeoutMs) {
  const controller = new AbortController();
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(() => {
        controller.signal.throwIfAborted();
        return operation(controller.signal);
      }),
      new Promise((_, reject) => {
        timer = setTimeout(() => {
          controller.abort();
          reject(new Error("probe deadline exceeded"));
        }, Math.max(0, timeoutMs));
      }),
    ]);
  } finally {
    clearTimeout(timer);
    controller.abort();
  }
}

async function readTargets(directory, signal) {
  const targets = new Map();
  const cursors = new Set();
  let cursor;
  do {
    signal.throwIfAborted();
    // This is the cell registry in the same DIRECTORY binding that serves
    // /v1/directory/:account. Never list or read acct: keys for probes.
    const page = await directory.list({ prefix: "cell:", cursor });
    signal.throwIfAborted();
    if (!Array.isArray(page?.keys) || typeof page.list_complete !== "boolean") {
      throw new Error("invalid probe directory");
    }
    await Promise.all(page.keys.map(async (key) => {
      signal.throwIfAborted();
      if (typeof key?.name !== "string" || !key.name.startsWith("cell:") ||
          !CELL_NAME.test(key.name.slice(5))) {
        throw new Error("invalid probe cell name");
      }
      const entry = await directory.get(key.name, { type: "json" });
      signal.throwIfAborted();
      if (!entry || typeof entry !== "object" || Array.isArray(entry)) {
        throw new Error("invalid probe cell record");
      }
      // Project only the key's cell name and endpoint. Credentials and other
      // registry fields never reach fetch options, persistence or exposition.
      targets.set(key.name.slice(5),
        typeof entry.endpoint === "string" ? entry.endpoint : "");
    }));
    cursor = page.list_complete ? undefined : page.cursor;
    if (!page.list_complete &&
        (typeof cursor !== "string" || !cursor || cursors.has(cursor))) {
      throw new Error("invalid probe directory cursor");
    }
    cursors.add(cursor);
  } while (cursor);
  return [...targets].map(([target, endpoint]) => ({ target, endpoint }));
}

function result(target, started, status = 0, skipped = false) {
  const checked = Date.now();
  const httpStatusClass = Number.isInteger(status) && status >= 100 && status < 600
    ? Math.floor(status / 100)
    : 0;
  return {
    target,
    state: skipped ? "skipped" : httpStatusClass === 2 ? "ok" : "down",
    http_status_class: httpStatusClass,
    latency_ms: skipped ? 0 : Math.max(0, checked - started),
    checked_at: new Date(checked).toISOString(),
  };
}

async function probe(target, endpoint, fetchImpl) {
  const started = Date.now();
  try {
    const url = new URL(endpoint);
    // Never forward URL credentials or query/fragment values from a registry
    // entry. Reject them instead of sending a possibly private URL to fetch.
    if (url.protocol !== "https:" || url.username || url.password ||
        url.search || url.hash) {
      return result(target, started);
    }
    url.pathname = `${url.pathname.replace(/\/+$/, "")}/v1/version`;
    const status = await bounded(async (signal) => {
      const response = await fetchImpl(url.href, {
        method: "GET",
        redirect: "manual",
        cache: "no-store",
        signal,
      });
      // Headers determine availability. Do not parse, retain or log bodies,
      // including error bodies; release the connection without awaiting data.
      if (response.body) void response.body.cancel().catch(() => {});
      return response.status;
    }, PROBE_TIMEOUT_MS);
    return result(target, started, status);
  } catch {
    return result(target, started);
  }
}

async function probeTargets(targets, fetchImpl, remaining) {
  const results = [];
  let next = 0;
  async function work() {
    while (next < targets.length) {
      const { target, endpoint } = targets[next++];
      // Waiting for a worker does not consume a request's timeout. Dispatch
      // only when the full timeout fits: a shortened run deadline cannot
      // establish that an endpoint is down.
      results.push(remaining() < PROBE_TIMEOUT_MS
        ? result(target, Date.now(), 0, true)
        : await probe(target, endpoint, fetchImpl));
    }
  }
  await Promise.all(Array.from({ length: Math.min(PROBE_CONCURRENCY, targets.length) }, work));
  return results;
}

export async function runScheduledUptimeProbes(env, fetchImpl = fetch) {
  const started = Date.now();
  const remaining = () => TOTAL_BUDGET_MS - (Date.now() - started);
  let directoryOk = false;
  let targetCount = 0;
  let recoveredFromMalformed = false;
  let results;
  try {
    // /v1/version renews Backend's 10m idle timer. Only an explicit opt-in may
    // run this container-reaching check; ordinary deployment preserves idle
    // shutdown. Disabled checks publish no fabricated success/failure sample.
    const cp = env.CP_UPTIME_PROBES_CONTROL_PLANE_ENABLED === "true"
      ? [{ target: CONTROL_PLANE_TARGET,
        endpoint: CONTROL_PLANE_URL.replace(/\/v1\/version$/, "") }]
      : [];
    const [discovery, snapshot] = await Promise.allSettled([
      bounded(
        (signal) => readTargets(env.DIRECTORY, signal), DIRECTORY_TIMEOUT_MS,
      ),
      bounded(
        (signal) => readRun(env.DIRECTORY, signal),
        SNAPSHOT_TIMEOUT_MS,
      ),
    ]);
    directoryOk = discovery.status === "fulfilled";
    const snapshotState = snapshot.status === "fulfilled" ? snapshot.value.state : "unreadable";
    recoveredFromMalformed = snapshotState === "malformed";
    const previous = snapshotState === "valid" ? snapshot.value.targets : [];
    const retained = directoryOk ? [] : previous
      .filter(({ target }) => target !== CONTROL_PLANE_TARGET)
      .map((row) => ({
        target: row.target, state: targetState(row),
        http_status_class: row.http_status_class,
        latency_ms: row.latency_ms, checked_at: row.checked_at,
        ...(row.last_observation ? { last_observation: completedObservation(row) } : {}),
      }));
    const targets = directoryOk ? discovery.value : [];
    // Count registry cells, so the optional self-probe cannot hide an empty
    // production directory. On discovery failure the retained count is advisory.
    targetCount = directoryOk ? targets.length : retained.length;
    const previousByTarget = new Map(previous.map((row) => [row.target, row]));
    const previouslyOk = new Set(previous.filter((row) => completedObservation(row)?.state === "ok")
      .map(({ target }) => target));
    const priority = ({ target }) => target === CONTROL_PLANE_TARGET ? 0 :
      targetState(previousByTarget.get(target) ?? {}) === "skipped" ? 1 :
      previouslyOk.has(target) ? 2 : 3;
    // Give skipped cells the next dispatch opportunities. Within that group,
    // never-checked cells go first, then the oldest completed observations;
    // a skip's own timestamp must not refresh its place in the queue.
    const lastChecked = ({ target }) => {
      const observation = completedObservation(previousByTarget.get(target));
      return observation ? Date.parse(observation.checked_at) : -Infinity;
    };
    const ordered = [...cp, ...targets].sort((a, b) => priority(a) - priority(b) ||
      (priority(a) === 1 ? lastChecked(a) - lastChecked(b) : 0) ||
      a.target.localeCompare(b.target));
    const probed = await probeTargets(ordered, fetchImpl,
      () => remaining() - WRITE_TIMEOUT_MS);
    for (const row of probed) {
      if (row.state !== "skipped") continue;
      const observation = completedObservation(previousByTarget.get(row.target));
      // Dispatch status must not erase an outage or refresh a check timestamp.
      // Carry only the last completed observation, without nesting skip history.
      if (observation) row.last_observation = observation;
    }
    // Valid observations survive skips above. Absent or malformed data has no
    // observations to protect: persist skips so those targets get the next turn.
    // Only a failed KV read leaves unknown evidence that cannot be replaced
    // without successful discovery and complete fresh probes.
    if (snapshotState !== "unreadable" || (directoryOk && probed.every((row) => row.state !== "skipped"))) {
      results = [...probed, ...retained].sort((a, b) => a.target.localeCompare(b.target));
    }
  } catch {
    // Never fail the probe event or log exceptions/registry data.
  }
  // Reserve the last five seconds for one atomic document write. No TTL: stopped
  // cron and failed persistence must leave timestamps visible for stale alerts.
  if (!results) return;
  const scheduler = {
    last_run_at: new Date(started).toISOString(),
    duration_ms: Math.max(0, Date.now() - started),
    target_count: targetCount,
    directory_ok: directoryOk,
  };
  try {
    await bounded(
      () => env.DIRECTORY.put(UPTIME_PROBE_RUN_KEY, JSON.stringify({
        scheduler, targets: results,
        ...(recoveredFromMalformed ? { recovered_from_malformed: true } : {}),
      })),
      Math.min(WRITE_TIMEOUT_MS, remaining()),
    );
  } catch {
    // A rejected write cannot advance the heartbeat independently of results.
  }
}

function targetState(row) {
  return row.state ?? (row.ok === true ? "ok" : row.ok === false ? "down" : undefined);
}

function completedObservation(row) {
  if (!row) return undefined;
  const observation = targetState(row) === "skipped" ? row.last_observation : row;
  if (!observation) return undefined;
  // Project the same value-free fields for both current and legacy snapshots.
  return {
    state: targetState(observation),
    http_status_class: observation.http_status_class,
    latency_ms: observation.latency_ms,
    checked_at: observation.checked_at,
  };
}

function validObservation(observation) {
  return observation && ["ok", "down"].includes(observation.state) &&
    Number.isInteger(observation.http_status_class) &&
    observation.http_status_class >= 0 && observation.http_status_class <= 5 &&
    (observation.state === "ok") === (observation.http_status_class === 2) &&
    Number.isFinite(observation.latency_ms) && observation.latency_ms >= 0 &&
    typeof observation.checked_at === "string" &&
    Number.isFinite(Date.parse(observation.checked_at)) &&
    observation.last_observation === undefined;
}

function validTargets(targets) {
  if (!Array.isArray(targets)) return false;
  const seen = new Set();
  for (const row of targets) {
    if (!row || typeof row.target !== "string" ||
        !(row.target === CONTROL_PLANE_TARGET || CELL_NAME.test(row.target)) ||
        seen.has(row.target) || !["ok", "down", "skipped"].includes(targetState(row)) ||
        !Number.isInteger(row.http_status_class) || row.http_status_class < 0 ||
        row.http_status_class > 5 || (targetState(row) === "ok") !== (row.http_status_class === 2) ||
        (targetState(row) === "skipped" && (row.http_status_class !== 0 || row.latency_ms !== 0)) ||
        (row.ok !== undefined && row.ok !== (targetState(row) === "ok")) ||
        (row.last_observation !== undefined && (targetState(row) !== "skipped" ||
          !validObservation(row.last_observation))) ||
        !Number.isFinite(row.latency_ms) || row.latency_ms < 0 ||
        typeof row.checked_at !== "string" ||
        !Number.isFinite(Date.parse(row.checked_at))) return false;
    seen.add(row.target);
  }
  return true;
}

function validScheduler(record) {
  return record && typeof record.last_run_at === "string" &&
    Number.isFinite(Date.parse(record.last_run_at)) &&
    Number.isFinite(record.duration_ms) && record.duration_ms >= 0 &&
    Number.isSafeInteger(record.target_count) && record.target_count >= 0 &&
    typeof record.directory_ok === "boolean";
}

// Read raw text so malformed JSON is distinguishable from a KV read failure.
// Missing keys permit migration; a present but invalid new document must never
// resurrect older legacy results or a separately advancing legacy heartbeat.
function parseRecord(raw) {
  try {
    return JSON.parse(raw);
  } catch {
    return undefined;
  }
}

async function readRun(directory, signal) {
  const invalid = { scheduler: null, targets: [], state: "malformed", writeOk: false };
  const raw = await directory.get(UPTIME_PROBE_RUN_KEY, { type: "text" });
  signal.throwIfAborted();
  if (raw !== null) {
    const document = parseRecord(raw);
    return validScheduler(document?.scheduler) && validTargets(document?.targets) &&
      (document.recovered_from_malformed === undefined || typeof document.recovered_from_malformed === "boolean")
      ? { scheduler: document.scheduler, targets: document.targets, state: "valid", writeOk: true,
        recoveredFromMalformed: document.recovered_from_malformed === true }
      : invalid;
  }
  // One-release compatibility with the previous two-key layout. Only the new
  // key is written; scheduler and targets always use this same read result.
  const [legacyTargets, legacyScheduler] = await Promise.all([
    directory.get(UPTIME_PROBES_KEY, { type: "text" }),
    directory.get(UPTIME_PROBE_SCHEDULER_KEY, { type: "text" }),
  ]);
  signal.throwIfAborted();
  const snapshot = parseRecord(legacyTargets);
  const scheduler = parseRecord(legacyScheduler);
  if ((legacyTargets !== null && (snapshot?.schema_version !== 1 || !validTargets(snapshot?.targets))) ||
      (legacyScheduler !== null && !validScheduler(scheduler))) return invalid;
  return {
    scheduler,
    targets: snapshot?.targets ?? [],
    state: legacyTargets === null && legacyScheduler === null ? "absent" : "valid",
    writeOk: legacyTargets !== null && legacyScheduler !== null,
  };
}

export async function uptimeProbeMetricsResponse(request, env) {
  const headers = {
    "Cache-Control": "no-store",
    "Content-Type": "text/plain; version=0.0.4; charset=utf-8",
  };
  if (request.method !== "GET") {
    return new Response("method not allowed\n", {
      status: 405, headers: { ...headers, Allow: "GET" },
    });
  }
  try {
    const { scheduler, targets, writeOk, recoveredFromMalformed = false } = await bounded(
      (signal) => readRun(env.DIRECTORY, signal), SNAPSHOT_TIMEOUT_MS,
    );
    const observations = targets.flatMap((row) => {
      const observation = completedObservation(row);
      return observation ? [{ target: row.target, ...observation }] : [];
    });
    const metrics = [
      ["witself_probe_up", "Whether the last completed probe returned HTTP 2xx.", (r) => Number(targetState(r) === "ok")],
      ["witself_probe_latency_seconds", "Last probe duration in seconds.", (r) => r.latency_ms / 1000],
      ["witself_probe_last_check_timestamp_seconds", "Last probe completion as Unix seconds.", (r) => Date.parse(r.checked_at) / 1000],
      ["witself_probe_http_status", "HTTP status class (1-5), or 0 without an HTTP response.", (r) => r.http_status_class],
    ];
    const lines = [];
    for (const [name, help, value] of [
      ["witself_probe_scheduler_last_run_timestamp_seconds", "Last scheduler run as Unix seconds, or 0 before its first record.", scheduler ? Date.parse(scheduler.last_run_at) / 1000 : 0],
      ["witself_probe_scheduler_target_count", "Registry cell count at the last run, retained on directory failure; excludes the optional self-probe.", scheduler?.target_count ?? 0],
      ["witself_probe_directory_ok", "Whether the last scheduler directory read succeeded.", Number(scheduler?.directory_ok ?? false)],
      ["witself_probe_snapshot_target_count", "Number of target rows in persisted results, including skipped targets and the optional self-probe.", targets.length],
      ["witself_probe_last_write_ok", "Whether KV contains a structurally valid run document (or complete legacy pair); 0 if missing or invalid on a successful KV read. Does not report the latest write attempt or freshness.", Number(writeOk)],
      ["witself_probe_document_was_malformed", "Whether the last persisted run replaced malformed stored data.", Number(recoveredFromMalformed)],
    ]) {
      lines.push(`# HELP ${name} ${help}`, `# TYPE ${name} gauge`, `${name} ${value}`);
    }
    for (const [name, help, value] of metrics) {
      lines.push(`# HELP ${name} ${help}`, `# TYPE ${name} gauge`);
      for (const row of observations) {
        // The closed cell-name alphabet excludes Prometheus label delimiters.
        lines.push(`${name}{target="${row.target}"} ${value(row)}`);
      }
    }
    lines.push("# HELP witself_probe_skipped Whether a target was skipped because the run's dispatch budget was exhausted.",
      "# TYPE witself_probe_skipped gauge");
    for (const row of targets.filter((row) => targetState(row) === "skipped")) {
      lines.push(`witself_probe_skipped{target="${row.target}"} 1`);
    }
    return new Response(`${lines.join("\n")}\n`, { headers });
  } catch {
    return new Response("probe results unavailable\n", { status: 503, headers });
  }
}
