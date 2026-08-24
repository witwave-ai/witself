import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import {
  consumeSignupCounter,
  parseSignupLimit,
  SIGNUP_GLOBAL_SCOPE,
  signupCounterDayKey,
  signupCounterScopes,
  signupIPScope,
  utcDayBucket,
} from "../src/signup-counters.mjs";

class Transaction {
  constructor(entries = {}) {
    this.values = new Map(
      Object.entries(entries).map(([key, value]) => [
        key,
        structuredClone(value),
      ]),
    );
    this.writes = 0;
  }

  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value);
  }

  async put(key, value) {
    this.writes++;
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.writes++;
    this.values.delete(key);
  }

  async list({ prefix = "" } = {}) {
    return new Map(
      [...this.values]
        .filter(([key]) => key.startsWith(prefix))
        .map(([key, value]) => [key, structuredClone(value)]),
    );
  }
}

test("UTC day buckets roll over independently of local offsets", () => {
  assert.equal(
    utcDayBucket(new Date("2026-08-24T23:59:59.999Z")),
    "2026-08-24",
  );
  assert.equal(
    utcDayBucket("2026-08-25T00:00:00.000Z"),
    "2026-08-25",
  );
  assert.equal(
    signupCounterDayKey("2026-08-25T00:00:00.000Z"),
    "count:2026-08-25",
  );
  assert.throws(() => utcDayBucket("not-a-date"), /time is invalid/);
});

test("only exact positive integer strings enable signup limits", () => {
  for (const value of [undefined, null, "", "0", "00", "01", " 1", "1 ", "+1", "-1", "1.5", 1, {}, "9007199254740992"]) {
    assert.equal(parseSignupLimit(value), 0, JSON.stringify(value));
  }
  assert.equal(parseSignupLimit("1"), 1);
  assert.equal(parseSignupLimit("300"), 300);
  assert.equal(
    parseSignupLimit(String(Number.MAX_SAFE_INTEGER)),
    Number.MAX_SAFE_INTEGER,
  );
});

test("IP scopes contain only the SHA-256 digest and global has one name", async () => {
  const sourceIP = "203.0.113.42";
  const digest = createHash("sha256").update(sourceIP).digest("hex");
  assert.equal(
    await signupIPScope(sourceIP),
    `signup-counter:ip:${digest}`,
  );
  assert.deepEqual(await signupCounterScopes(sourceIP), {
    ip: `signup-counter:ip:${digest}`,
    global: "signup-counter:global",
  });
  assert.equal(SIGNUP_GLOBAL_SCOPE, "signup-counter:global");
  await assert.rejects(
    signupIPScope(sourceIP, async () => sourceIP),
    /hash is invalid/,
  );
});

test("counter consumption is exact, bounded, and returns prior verdicts", async () => {
  const transaction = new Transaction();
  const now = new Date("2026-08-24T12:00:00.000Z");
  const consume = (provisionId, limit = 2) =>
    consumeSignupCounter(transaction, { provisionId, limit, now });

  assert.deepEqual(await consume("provision-a"), {
    allowed: true,
    count: 1,
    limit: 2,
    day: "2026-08-24",
    replayed: false,
  });
  assert.deepEqual(
    await consume("provision-a", 1),
    {
      allowed: true,
      count: 1,
      limit: 2,
      day: "2026-08-24",
      replayed: true,
    },
    "a replay returns the committed verdict even if the binding changed",
  );
  assert.equal((await consume("provision-b")).count, 2);
  assert.deepEqual(await consume("provision-c"), {
    allowed: false,
    count: 2,
    limit: 2,
    day: "2026-08-24",
    replayed: false,
  });
  assert.equal(transaction.values.has("use:provision-c"), false);
  assert.deepEqual(await consume("provision-c", 3), {
    allowed: true,
    count: 3,
    limit: 3,
    day: "2026-08-24",
    replayed: false,
  });
  assert.deepEqual(transaction.values.get("count:2026-08-24"), {
    day: "2026-08-24",
    count: 3,
  });
});

test("the first new consume lazily prunes prior-day count and use keys", async () => {
  const transaction = new Transaction({
    "active-day": "2026-08-23",
    "count:2026-08-23": { day: "2026-08-23", count: 9 },
    "use:old-provision": {
      allowed: true,
      count: 9,
      limit: 10,
      day: "2026-08-23",
    },
  });
  const verdict = await consumeSignupCounter(transaction, {
    provisionId: "new-provision",
    limit: 10,
    now: new Date("2026-08-24T00:00:01.000Z"),
  });
  assert.equal(verdict.allowed, true);
  assert.equal(transaction.values.has("count:2026-08-23"), false);
  assert.equal(transaction.values.has("use:old-provision"), false);
  assert.equal(transaction.values.get("active-day"), "2026-08-24");
  assert.deepEqual(transaction.values.get("count:2026-08-24"), {
    day: "2026-08-24",
    count: 1,
  });
});

test("disabled limits are inert and require no transaction", async () => {
  assert.deepEqual(await consumeSignupCounter(null, {
    provisionId: "dark-default",
    limit: 0,
    now: new Date("2026-08-24T12:00:00.000Z"),
  }), {
    allowed: true,
    count: 0,
    limit: 0,
    day: "2026-08-24",
    replayed: false,
    disabled: true,
  });
});
