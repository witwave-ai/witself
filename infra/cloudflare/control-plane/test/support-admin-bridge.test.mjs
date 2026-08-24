import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;

const ORIGIN = "https://cp.test.invalid";
const CELL_ENDPOINT = "https://cell.test.invalid";
const ACCOUNT = "acct_support";
const TICKET = "tkt_abc123";
const ADMIN_TOKEN = "witself_adm_test-token";
const ADMIN_ID = "adm_abcdefghijklmnopqrst";
const ADMIN_HANDLE = "sarah";
const PROVISION_TOKEN = "provision-token";

async function sha256hex(value) {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(value),
  );
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

class KVFake {
  constructor(values) {
    this.values = new Map(
      Object.entries(values).map(([key, value]) => [key, JSON.stringify(value)]),
    );
  }

  async get(key, options) {
    const value = this.values.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async put(key, value) {
    this.values.set(key, value);
  }
}

async function testEnv() {
  const tokenHash = await sha256hex(ADMIN_TOKEN);
  return {
    DIRECTORY: new KVFake({
      [`admintok:${tokenHash}`]: { admin_id: ADMIN_ID },
      [`admin:${ADMIN_ID}`]: {
        admin_id: ADMIN_ID,
        handle: ADMIN_HANDLE,
      },
      [`acct:${ACCOUNT}`]: { cell: "cell-a" },
      "cell:cell-a": {
        endpoint: CELL_ENDPOINT,
        provision_token: PROVISION_TOKEN,
      },
    }),
  };
}

function adminRequest(path, method, body) {
  return new Request(`${ORIGIN}${path}`, {
    method,
    headers: {
      Authorization: `Bearer ${ADMIN_TOKEN}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
  });
}

test("ticket reply and retriage routes preserve the strict cell bridge contract", async (t) => {
  const env = await testEnv();
  const upstream = [];
  const emails = [];
  env.EMAIL = { send: async (message) => emails.push(message) };
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/${ACCOUNT}:contact`)) {
      return Response.json({ status: "active", email: "owner@example.test" });
    }
    upstream.push({ url, init, body: JSON.parse(init.body) });
    if (url.endsWith("admin:reply-ticket")) {
      return Response.json({
        message: {
          id: `msg_${upstream.length}`,
          author_kind: "assistant",
          body: "A reply",
        },
      });
    }
    return Response.json({ ticket: { id: TICKET } });
  };
  t.after(() => {
    globalThis.fetch = originalFetch;
  });
  const waitUntil = [];
  const ctx = { waitUntil: (promise) => waitUntil.push(promise) };

  const replyResponse = await worker.fetch(
    adminRequest(
      `/v1/admin/accounts/${ACCOUNT}/tickets/${TICKET}/messages`,
      "POST",
      { body: "A reply", as_assistant: true },
    ),
    env,
    ctx,
  );
  assert.equal(replyResponse.status, 200);
  assert.equal(
    upstream[0].url,
    `${CELL_ENDPOINT}/v1/accounts/${ACCOUNT}/admin:reply-ticket`,
  );
  assert.deepEqual(upstream[0].body, {
    admin_handle: ADMIN_HANDLE,
    ticket_id: TICKET,
    body: "A reply",
    as_assistant: true,
  });
  assert.equal(upstream[0].init.method, "POST");
  assert.equal(
    upstream[0].init.headers.Authorization,
    `Bearer ${PROVISION_TOKEN}`,
  );
  await Promise.all(waitUntil.splice(0));
  assert.equal(emails.length, 1);
  assert.match(
    emails[0].text,
    /^The Witself support assistant replied to your ticket\./,
  );
  assert.ok(!emails[0].text.includes(ADMIN_HANDLE));
  assert.ok(!emails[0].html.includes(ADMIN_HANDLE));

  const nonBooleanResponse = await worker.fetch(
    adminRequest(
      `/v1/admin/accounts/${ACCOUNT}/tickets/${TICKET}/messages`,
      "POST",
      { body: "A human reply", as_assistant: "true" },
    ),
    env,
    ctx,
  );
  assert.equal(nonBooleanResponse.status, 200);
  assert.equal(upstream[1].body.as_assistant, false);

  const retriageResponse = await worker.fetch(
    adminRequest(
      `/v1/admin/accounts/${ACCOUNT}/tickets/${TICKET}/retriage`,
      "PATCH",
      { category: "security", priority: "urgent" },
    ),
    env,
    ctx,
  );
  assert.equal(retriageResponse.status, 200);
  assert.equal(
    upstream[2].url,
    `${CELL_ENDPOINT}/v1/accounts/${ACCOUNT}/admin:retriage-ticket`,
  );
  assert.equal(upstream[2].init.method, "POST");
  assert.equal(
    upstream[2].init.headers.Authorization,
    `Bearer ${PROVISION_TOKEN}`,
  );
  assert.deepEqual(upstream[2].body, {
    admin_handle: ADMIN_HANDLE,
    ticket_id: TICKET,
    category: "security",
    priority: "urgent",
  });

  await Promise.all(waitUntil);
});
