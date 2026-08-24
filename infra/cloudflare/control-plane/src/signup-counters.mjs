const ACTIVE_DAY_KEY = "active-day";
const COUNT_PREFIX = "count:";
const USE_PREFIX = "use:";
const PROVISION_ID = /^[A-Za-z0-9_-]{1,128}$/;
const SHA256_HEX = /^[0-9a-f]{64}$/;

export const SIGNUP_GLOBAL_SCOPE = "signup-counter:global";

// utcDayBucket deliberately derives the calendar day through ISO UTC rather
// than local date fields. Every counter object therefore rolls over at the
// same instant, independent of the Worker's deployment region.
export function utcDayBucket(now = new Date()) {
  const date = now instanceof Date ? now : new Date(now);
  if (!Number.isFinite(date.getTime())) {
    throw new TypeError("signup counter time is invalid");
  }
  return date.toISOString().slice(0, 10);
}

export function signupCounterDayKey(now = new Date()) {
  return `${COUNT_PREFIX}${utcDayBucket(now)}`;
}

// Only canonical, positive base-10 integer strings enable a limit. In
// particular, absent values and the rendered dark default "0" are inert;
// whitespace, signs, decimals, and values outside the safe integer range do
// not accidentally turn a malformed binding into an enforcement policy.
export function parseSignupLimit(value) {
  if (value == null || value === "0") return 0;
  if (typeof value !== "string" || !/^[1-9][0-9]*$/.test(value)) {
    return 0;
  }
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : 0;
}

async function sha256Hex(value) {
  const bytes = new TextEncoder().encode(value);
  const digest = await globalThis.crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

export async function signupIPScope(sourceIP, hash = sha256Hex) {
  if (typeof sourceIP !== "string") {
    throw new TypeError("signup source ip must be a string");
  }
  const digest = await hash(sourceIP);
  if (!SHA256_HEX.test(digest ?? "")) {
    throw new TypeError("signup source ip hash is invalid");
  }
  return `signup-counter:ip:${digest}`;
}

export async function signupCounterScopes(sourceIP, hash = sha256Hex) {
  return {
    ip: await signupIPScope(sourceIP, hash),
    global: SIGNUP_GLOBAL_SCOPE,
  };
}

function markerVerdict(marker) {
  if (
    !marker ||
    typeof marker !== "object" ||
    typeof marker.allowed !== "boolean" ||
    !Number.isSafeInteger(marker.count) ||
    marker.count < 0 ||
    !Number.isSafeInteger(marker.limit) ||
    marker.limit < 1 ||
    typeof marker.day !== "string"
  ) {
    throw new TypeError("signup counter marker is invalid");
  }
  return {
    allowed: marker.allowed,
    count: marker.count,
    limit: marker.limit,
    day: marker.day,
    replayed: true,
  };
}

async function prunePriorDayKeys(transaction, day) {
  const activeDay = await transaction.get(ACTIVE_DAY_KEY);
  if (activeDay === day) return;

  const staleKeys = [];
  const counts = await transaction.list({ prefix: COUNT_PREFIX });
  for (const key of counts.keys()) {
    if (key !== `${COUNT_PREFIX}${day}`) staleKeys.push(key);
  }
  const uses = await transaction.list({ prefix: USE_PREFIX });
  for (const [key, marker] of uses) {
    if (marker?.day !== day) staleKeys.push(key);
  }
  for (const key of staleKeys) {
    await transaction.delete(key);
  }
  await transaction.put(ACTIVE_DAY_KEY, day);
}

// consumeSignupCounter is the complete Durable Object transaction body. A
// committed marker makes an ambiguous self-call retry return the first exact
// verdict instead of charging the same provision twice (or changing a deny
// into an allow after an operator changes the configured limit).
export async function consumeSignupCounter(transaction, {
  provisionId,
  limit,
  now = new Date(),
}) {
  if (!PROVISION_ID.test(provisionId ?? "")) {
    throw new TypeError("signup counter provision id is invalid");
  }
  if (limit === 0) {
    return {
      allowed: true,
      count: 0,
      limit: 0,
      day: utcDayBucket(now),
      replayed: false,
      disabled: true,
    };
  }
  if (!Number.isSafeInteger(limit) || limit < 1) {
    throw new TypeError("signup counter limit is invalid");
  }
  if (
    !transaction ||
    typeof transaction.get !== "function" ||
    typeof transaction.put !== "function" ||
    typeof transaction.delete !== "function" ||
    typeof transaction.list !== "function"
  ) {
    throw new TypeError("signup counter transaction is unavailable");
  }

  const useKey = `${USE_PREFIX}${provisionId}`;
  const existing = await transaction.get(useKey);
  if (existing !== undefined && existing !== null) {
    return markerVerdict(existing);
  }

  const day = utcDayBucket(now);
  await prunePriorDayKeys(transaction, day);
  const countKey = `${COUNT_PREFIX}${day}`;
  const countState = await transaction.get(countKey);
  const count = countState === undefined || countState === null
    ? 0
    : countState?.day === day &&
        Number.isSafeInteger(countState.count) &&
        countState.count >= 0
    ? countState.count
    : null;
  if (count === null) {
    throw new TypeError("signup counter state is invalid");
  }

  const allowed = count < limit;
  const nextCount = allowed ? count + 1 : count;
  const marker = {
    allowed,
    count: nextCount,
    limit,
    day,
  };
  if (allowed) {
    await transaction.put(countKey, { day, count: nextCount });
    await transaction.put(useKey, marker);
  }
  return { ...marker, replayed: false };
}
