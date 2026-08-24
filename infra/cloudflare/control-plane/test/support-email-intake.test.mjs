import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";

if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value(left, right) {
      return nodeTimingSafeEqual(Buffer.from(left), Buffer.from(right));
    },
  });
}

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;

const ORIGIN = "https://cp.test.invalid";
const ROUTE = "/v1/intake/support-email";
const TOKEN = "support-intake-token";
const ACCOUNT = "acct_support_email";
const CELL_ENDPOINT = "https://cell-a.test.invalid";
const PROVISION_TOKEN = "provision-token-a";
const MESSAGE_ID = "<support-message-1@example.test>";
const TICKET_TAG = "tkt_abcdefghijklmnop";

class KVFake {
  constructor(values = {}) {
    this.values = new Map(Object.entries(values).map(([key, value]) => [
      key,
      typeof value === "string" ? value : JSON.stringify(value),
    ]));
    this.puts = [];
  }

  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async put(key, value, options) {
    this.puts.push({ key, value, options });
    this.values.set(key, value);
  }

  async list({ prefix = "" }) {
    return {
      keys: [...this.values.keys()]
        .filter((key) => key.startsWith(prefix))
        .map((name) => ({ name })),
      list_complete: true,
    };
  }
}

function payload(overrides = {}) {
  return {
    sender: "owner@example.test",
    subject: "Need help",
    body: "Please help with this issue.",
    message_id: MESSAGE_ID,
    ...overrides,
  };
}

function request(body = payload(), token = TOKEN) {
  return new Request(`${ORIGIN}${ROUTE}`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  });
}

function envWithCells({
  enabled = "true",
  cells = [{
    name: "cell-a",
    endpoint: CELL_ENDPOINT,
    provision_token: PROVISION_TOKEN,
  }],
  directory = {},
} = {}) {
  const values = { ...directory };
  for (const cell of cells) {
    values[`cell:${cell.name}`] = {
      endpoint: cell.endpoint,
      provision_token: cell.provision_token,
    };
  }
  return {
    CP_SUPPORT_EMAIL_INTAKE_ENABLED: enabled,
    SUPPORT_EMAIL_INTAKE_TOKEN: TOKEN,
    DIRECTORY: new KVFake(values),
  };
}

function stubFetch(t, implementation) {
  const original = globalThis.fetch;
  globalThis.fetch = implementation;
  t.after(() => {
    globalThis.fetch = original;
  });
}

async function disposition(response) {
  assert.equal(response.status, 200);
  const value = await response.json();
  assert.deepEqual(Object.keys(value).sort(), ["disposition", "schema_version"]);
  assert.equal(value.schema_version, "witself.v0");
  return value.disposition;
}

test("support-email intake stays dark and rejects a bad bridge bearer", async () => {
  const dark = envWithCells({ enabled: "false" });
  assert.equal((await worker.fetch(request(), dark, {})).status, 404);

  const enabled = envWithCells();
  assert.equal(
    (await worker.fetch(request(payload(), "wrong-token"), enabled, {})).status,
    401,
  );
  assert.equal(enabled.DIRECTORY.puts.length, 0);
});

test("support-email intake reserves briefly and deduplicates terminal outcomes for seven days", async (t) => {
  const env = envWithCells();
  let fetches = 0;
  stubFetch(t, async () => {
    fetches += 1;
    return Response.json({ matches: [] });
  });

  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "drop_unmatched",
  );
  assert.equal(fetches, 1);
  assert.equal(env.DIRECTORY.puts.length, 2);
  assert.match(
    env.DIRECTORY.puts[0].key,
    /^intake_dedup:[0-9a-f]{64}:pending$/,
  );
  assert.deepEqual(env.DIRECTORY.puts[0].options, {
    expirationTtl: 60,
  });
  assert.equal(env.DIRECTORY.puts[0].value, "pending");
  assert.equal(
    env.DIRECTORY.puts[1].key,
    env.DIRECTORY.puts[0].key.slice(0, -":pending".length),
  );
  assert.equal(env.DIRECTORY.puts[1].value, "done");
  assert.deepEqual(env.DIRECTORY.puts[1].options, {
    expirationTtl: 7 * 24 * 60 * 60,
  });

  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "duplicate",
  );
  assert.equal(fetches, 1);
});

test("support-email intake keeps failed cell delivery retryable", async (t) => {
  const env = envWithCells({
    directory: { [`acct:${ACCOUNT}`]: { cell: "cell-a" } },
  });
  let cellAttempts = 0;
  stubFetch(t, async (input) => {
    if (String(input).endsWith("admin:match-contact")) {
      return Response.json({
        schema_version: "witself.v0",
        matches: [{ account_id: ACCOUNT, status: "active" }],
      });
    }
    cellAttempts += 1;
    if (cellAttempts === 1) {
      return Response.json({ error: "transient" }, { status: 503 });
    }
    return Response.json({ ticket: { id: TICKET_TAG } });
  });

  assert.equal((await worker.fetch(request(), env, {})).status, 502);
  const pendingKey = env.DIRECTORY.puts[0].key;
  assert.equal(env.DIRECTORY.values.get(pendingKey), "pending");

  // While the short reservation is live, CP refuses to acknowledge the
  // replay. The bridge converts this response into a provider retry.
  assert.equal((await worker.fetch(request(), env, {})).status, 503);
  assert.equal(cellAttempts, 1);

  // Simulate the 60-second KV lease expiring, then prove the same Message-ID
  // can reach the idempotent cell route and become terminal.
  env.DIRECTORY.values.delete(pendingKey);
  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "opened",
  );
  assert.equal(cellAttempts, 2);
  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "duplicate",
  );
  assert.equal(cellAttempts, 2);
});

test("support-email intake retries when terminal dedup persistence fails", async (t) => {
  const env = envWithCells({
    directory: { [`acct:${ACCOUNT}`]: { cell: "cell-a" } },
  });
  const originalPut = env.DIRECTORY.put.bind(env.DIRECTORY);
  let failTerminalPut = true;
  env.DIRECTORY.put = async (key, value, options) => {
    if (value === "done" && failTerminalPut) {
      failTerminalPut = false;
      throw new Error("simulated terminal KV failure");
    }
    return originalPut(key, value, options);
  };
  let cellAttempts = 0;
  stubFetch(t, async (input) => {
    if (String(input).endsWith("admin:match-contact")) {
      return Response.json({
        schema_version: "witself.v0",
        matches: [{ account_id: ACCOUNT, status: "active" }],
      });
    }
    cellAttempts += 1;
    return Response.json({ ticket: { id: TICKET_TAG } });
  });

  // The cell succeeded, but CP must not acknowledge the email until the
  // seven-day terminal marker is durable.
  assert.equal((await worker.fetch(request(), env, {})).status, 503);
  const pendingKey = env.DIRECTORY.puts[0].key;
  const key = pendingKey.slice(0, -":pending".length);
  assert.equal(env.DIRECTORY.values.get(pendingKey), "pending");
  assert.equal((await worker.fetch(request(), env, {})).status, 503);
  assert.equal(cellAttempts, 1);

  // After the short lease, the replay reaches the cell again. The real cell
  // returns the existing Message-ID result, so this converges without a
  // duplicate ticket or message and can persist the terminal marker.
  env.DIRECTORY.values.delete(pendingKey);
  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "opened",
  );
  assert.equal(cellAttempts, 2);
  assert.equal(env.DIRECTORY.values.get(key), "done");
});

test("support-email intake drops unmatched, ambiguous, and broken fan-out", async (t) => {
  const cells = [
    {
      name: "cell-a",
      endpoint: "https://cell-a.test.invalid",
      provision_token: "token-a",
    },
    {
      name: "cell-b",
      endpoint: "https://cell-b.test.invalid",
      provision_token: "token-b",
    },
  ];
  const ambiguous = envWithCells({ cells });
  const broken = envWithCells({ cells });
  stubFetch(t, async (input) => {
    const url = String(input);
    if (url.startsWith("https://cell-b") &&
        broken.DIRECTORY.puts.length > 0 &&
        ambiguous.DIRECTORY.puts.length > 0) {
      return Response.json({ error: "unavailable" }, { status: 503 });
    }
    return Response.json({
      schema_version: "witself.v0",
      matches: [{ account_id: ACCOUNT, status: "active" }],
    });
  });

  assert.equal(
    await disposition(await worker.fetch(request(), ambiguous, {})),
    "drop_ambiguous",
  );
  assert.equal(
    await disposition(await worker.fetch(
      request(payload({ message_id: "<support-message-2@example.test>" })),
      broken,
      {},
    )),
    "drop_fanout_error",
  );
});

test("support-email intake drops an archived matched account", async (t) => {
  const env = envWithCells({
    directory: {
      [`archived:${ACCOUNT}`]: { cell: "cell-a" },
    },
  });
  let calls = 0;
  stubFetch(t, async () => {
    calls += 1;
    return Response.json({
      schema_version: "witself.v0",
      matches: [{ account_id: ACCOUNT, status: "active" }],
    });
  });
  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "drop_archived",
  );
  assert.equal(calls, 1);
});

test("support-email intake treats an incompletely registered cell as fan-out failure", async (t) => {
  const env = envWithCells({
    cells: [{
      name: "cell-a",
      endpoint: CELL_ENDPOINT,
      provision_token: "",
    }],
  });
  stubFetch(t, async () => {
    throw new Error("an incomplete cell must not be queried");
  });
  assert.equal(
    await disposition(await worker.fetch(request(), env, {})),
    "drop_fanout_error",
  );
});

test("support-email intake opens a ticket with the exact cell contract", async (t) => {
  const env = envWithCells({
    directory: { [`acct:${ACCOUNT}`]: { cell: "cell-a" } },
  });
  const calls = [];
  stubFetch(t, async (input, init) => {
    const call = { url: String(input), init, body: JSON.parse(init.body) };
    calls.push(call);
    if (call.url.endsWith("admin:match-contact")) {
      return Response.json({
        schema_version: "witself.v0",
        matches: [{ account_id: ACCOUNT, status: "active" }],
      });
    }
    return Response.json({ ticket: { id: TICKET_TAG } });
  });

  const response = await worker.fetch(request(), env, {});
  const responseText = await response.clone().text();
  assert.equal(await disposition(response), "opened");
  assert.equal(
    calls[0].url,
    `${CELL_ENDPOINT}/v1/support/admin:match-contact`,
  );
  assert.deepEqual(calls[0].body, { email: payload().sender });
  assert.equal(
    calls[1].url,
    `${CELL_ENDPOINT}/v1/accounts/${ACCOUNT}/admin:open-email-ticket`,
  );
  assert.equal(calls[1].init.headers.Authorization, `Bearer ${PROVISION_TOKEN}`);
  assert.deepEqual(calls[1].body, {
    email: payload().sender,
    subject: payload().subject,
    body: payload().body,
    email_message_id: MESSAGE_ID,
  });
  assert.doesNotMatch(responseText, /owner@example|Need help|Please help/);
});

test("tagged support email replies and 404 or 409 falls back to open", async (t) => {
  const makeEnv = () => envWithCells({
    directory: { [`acct:${ACCOUNT}`]: { cell: "cell-a" } },
  });
  const calls = [];
  const logs = [];
  const originalLog = console.log;
  console.log = (...values) => logs.push(values.join(" "));
  t.after(() => {
    console.log = originalLog;
  });
  stubFetch(t, async (input, init) => {
    const call = { url: String(input), init, body: JSON.parse(init.body) };
    calls.push(call);
    if (call.url.endsWith("admin:match-contact")) {
      return Response.json({
        schema_version: "witself.v0",
        matches: [{ account_id: ACCOUNT, status: "active" }],
      });
    }
    if (call.url.endsWith("admin:reply-email-ticket") &&
        call.body.email_message_id.includes("fallback")) {
      const status = call.body.email_message_id.includes("404") ? 404 : 409;
      return Response.json({ error: "fallback" }, { status });
    }
    return Response.json({ message: { id: "stm_test" } });
  });

  const tagged = payload({ ticket_tag: TICKET_TAG });
  assert.equal(
    await disposition(await worker.fetch(request(tagged), makeEnv(), {})),
    "replied",
  );
  assert.equal(
    calls[1].url,
    `${CELL_ENDPOINT}/v1/accounts/${ACCOUNT}/admin:reply-email-ticket`,
  );
  assert.deepEqual(calls[1].body, {
    email: tagged.sender,
    ticket_id: TICKET_TAG,
    body: tagged.body,
    email_message_id: MESSAGE_ID,
  });

  for (const status of [404, 409]) {
    const fallback = payload({
      message_id: `<support-fallback-${status}@example.test>`,
      ticket_tag: TICKET_TAG,
    });
    assert.equal(
      await disposition(await worker.fetch(request(fallback), makeEnv(), {})),
      "opened",
    );
    assert.match(calls.at(-2).url, /admin:reply-email-ticket$/);
    assert.match(calls.at(-1).url, /admin:open-email-ticket$/);
    assert.deepEqual(calls.at(-1).body, {
      email: fallback.sender,
      subject: fallback.subject,
      body: fallback.body,
      email_message_id: fallback.message_id,
    });
  }
  assert.deepEqual(logs, [
    "support-email-intake disposition=replied",
    "support-email-intake disposition=opened",
    "support-email-intake disposition=opened",
  ]);
  assert.doesNotMatch(
    logs.join("\n"),
    /owner@example|support-message|support-fallback|Need help|Please help|acct_|tkt_/,
  );
});
