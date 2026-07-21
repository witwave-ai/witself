import assert from "node:assert/strict";
import test from "node:test";

import { canaryConfiguration, runCanary } from "../scripts/canary.mjs";

const config = {
  accountID: "a".repeat(32),
  cloudflareToken: "cloudflare-token",
  from: "canary@sender.example",
  to: "canary.realm@example.net",
  endpoint: "https://cell.example.test",
  witselfToken: "witself-token",
  timeoutSeconds: 20,
};

const retryChallenge = "11111111-2222-4333-8444-555555555555";
const correlationNonce = "66666666-7777-4888-8999-aaaaaaaaaaaa";
const claimKey = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee";
const completeKey = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
const messageID = "emsg_aaaaaaaaaaaaaaaa";
const claimID = "ecl_bbbbbbbbbbbbbbbb";
const code = "731204";
const subject = `Witself receive canary ${correlationNonce}`;

function canaryUUIDGenerator() {
  const values = [retryChallenge, correlationNonce, claimKey, completeKey];
  return () => values.shift() ?? completeKey;
}

function successfulCanaryFetch({ calls, list }) {
  let statusCalls = 0;
  return async (url, init) => {
    calls.push({ url, ...init });
    const parsed = new URL(url);
    const path = parsed.pathname;
    if (path.endsWith("/v1/email/retry-canary:arm")) {
      return Response.json({ checkpoint: {
        state: "armed", armed: true, tempfailed: false, accepted: false, tempfail_count: 0,
      } });
    }
    if (path.includes("/email/sending/send")) {
      return Response.json({ success: true, result: { delivered: [config.to], queued: [] } });
    }
    if (path.endsWith("/v1/email/retry-canary:status")) {
      statusCalls += 1;
      return Response.json({ checkpoint: statusCalls === 1 ? {
        state: "tempfailed", armed: true, tempfailed: true, accepted: false, tempfail_count: 1,
      } : {
        state: "accepted", armed: true, tempfailed: true, accepted: true, tempfail_count: 1,
      } });
    }
    if (path === "/v1/email") {
      return Response.json(list(parsed));
    }
    if (path.endsWith(`/${messageID}:claim`)) {
      return Response.json({ processing: { state: "claimed", claim_id: claimID, generation: 1 } });
    }
    if (path.endsWith(`/${messageID}:read`)) {
      return Response.json({ message: {
        id: messageID,
        parse_state: "parsed",
        subject,
        text: `Synthetic monitoring message. Verification code: ${code}.`,
      } });
    }
    return Response.json({ message: { id: messageID }, processing: { state: "completed" } });
  };
}

async function runSuccessfulCanary(fetch) {
  let tick = 1000;
  return runCanary(config, {
    fetch,
    randomUUID: canaryUUIDGenerator(),
    randomCode: () => Number(code),
    now: () => { tick += 50; return tick; },
    sleep: async () => {},
  });
}

test("canary completes the full value-free owner lifecycle", async () => {
  const calls = [];
  const fetch = successfulCanaryFetch({
    calls,
    list: () => ({ messages: [{ id: messageID, subject }], next_cursor: "" }),
  });
  const result = await runSuccessfulCanary(fetch);

  assert.equal(result.outcome, "passed");
  assert.equal(result.code_value_returned, false);
  assert.equal(result.content_returned, false);
  assert.equal(result.identifiers_returned, false);
  assert.equal(result.provider_retry_proven, true);
  assert.deepEqual(calls.map((call) => new URL(call.url).pathname), [
    "/v1/email/retry-canary:arm",
    `/client/v4/accounts/${config.accountID}/email/sending/send`,
    "/v1/email/retry-canary:status",
    "/v1/email/retry-canary:status",
    "/v1/email",
    `/v1/email/${messageID}:claim`,
    `/v1/email/${messageID}:read`,
    `/v1/email/${messageID}:code-consumed`,
    `/v1/email/${messageID}:complete`,
    `/v1/email/${messageID}:ack`,
  ]);
  for (const call of calls.filter((call) => !call.url.includes("/email/sending/send"))) {
    assert.equal(call.headers.Authorization, `Bearer ${config.witselfToken}`);
  }

  const arm = calls.find((call) => call.url.endsWith("/v1/email/retry-canary:arm"));
  const statuses = calls.filter((call) => call.url.endsWith("/v1/email/retry-canary:status"));
  assert.deepEqual(JSON.parse(arm.body), { challenge: retryChallenge });
  assert.equal(statuses.every((call) => JSON.parse(call.body).challenge === retryChallenge), true);

  const send = calls.find((call) => call.url.includes("/email/sending/send"));
  const payload = JSON.parse(send.body);
  assert.equal(payload.headers["X-Witself-Canary-Retry"], retryChallenge);
  assert.equal(payload.subject, subject);
  assert.match(payload.subject, new RegExp(correlationNonce));
  assert.doesNotMatch(payload.subject, new RegExp(retryChallenge));
  assert.doesNotMatch(payload.text, new RegExp(retryChallenge));

  const listCall = calls.find((call) => new URL(call.url).pathname === "/v1/email");
  const listURL = new URL(listCall.url);
  assert.equal(listCall.method, "GET");
  assert.equal(listURL.searchParams.get("unacked"), "true");
  assert.equal(listURL.searchParams.get("oldest_first"), "false");
  assert.equal(listURL.searchParams.get("limit"), "100");
  assert.equal(listURL.searchParams.has("cursor"), false);

  for (const call of calls) {
    assert.ok(call.signal instanceof AbortSignal);
  }
  assert.doesNotMatch(JSON.stringify(result), new RegExp(code));
  assert.doesNotMatch(JSON.stringify(result), new RegExp(messageID));
  assert.doesNotMatch(JSON.stringify(result), new RegExp(retryChallenge));
  assert.doesNotMatch(JSON.stringify(result), new RegExp(correlationNonce));
});

test("canary discovers its message behind more than 100 unacked messages", async () => {
  const calls = [];
  const cursor = "opaque cursor/for page 2";
  const backlog = Array.from({ length: 100 }, () => ({
    id: "emsg_cccccccccccccccc",
    subject: "unrelated backlog message",
  }));
  const fetch = successfulCanaryFetch({
    calls,
    list: (url) => url.searchParams.has("cursor")
      ? { messages: [{ id: messageID, subject }], next_cursor: "" }
      : { messages: backlog, next_cursor: cursor },
  });

  const result = await runSuccessfulCanary(fetch);
  assert.equal(result.outcome, "passed");
  const listCalls = calls.filter((call) => new URL(call.url).pathname === "/v1/email");
  assert.equal(listCalls.length, 2);
  assert.equal(new URL(listCalls[0].url).searchParams.has("cursor"), false);
  assert.equal(new URL(listCalls[1].url).searchParams.get("cursor"), cursor);
  assert.equal(listCalls.every((call) => call.method === "GET"), true);
});

test("canary rejects an unsafe email list cursor", async () => {
  const calls = [];
  const fetch = successfulCanaryFetch({
    calls,
    list: () => ({ messages: [], next_cursor: "bad\nopaque-cursor" }),
  });
  await assert.rejects(() => runSuccessfulCanary(fetch), /email list cursor was invalid/);
});

test("canary releases the exact fence after a post-claim failure", async () => {
  const paths = [];
  const fetch = async (url, init) => {
    const parsed = new URL(url);
    const path = parsed.pathname;
    paths.push(path);
    if (path.endsWith("/v1/email/retry-canary:arm")) {
      return Response.json({ checkpoint: {
        state: "armed", armed: true, tempfailed: false, accepted: false, tempfail_count: 0,
      } });
    }
    if (path.includes("/email/sending/send")) {
      return Response.json({ success: true, result: { queued: [config.to] } });
    }
    if (path.endsWith("/v1/email/retry-canary:status")) {
      return Response.json({ checkpoint: {
        state: "accepted", armed: true, tempfailed: true, accepted: true, tempfail_count: 1,
      } });
    }
    if (path === "/v1/email") {
      return Response.json({ messages: [{ id: messageID, subject }], next_cursor: "" });
    }
    if (path.endsWith(":claim")) {
      return Response.json({ processing: {
        state: "claimed", claim_id: claimID, generation: 4,
      } });
    }
    if (path.endsWith(":read")) {
      return Response.json({ message: { parse_state: "error" } });
    }
    if (path.endsWith(":release")) {
      const body = JSON.parse(init.body);
      assert.deepEqual(body, { claim_id: claimID, generation: 4 });
      return Response.json({ processing: { state: "available" } });
    }
    throw new Error(`unexpected ${path}`);
  };
  await assert.rejects(() => runCanary(config, {
    fetch,
    randomUUID: canaryUUIDGenerator(),
    randomCode: () => Number(code),
  }), /content did not round-trip/);
  assert.equal(paths.at(-1), `/v1/email/${messageID}:release`);
});

test("canary configuration requires bounded credential-free inputs", () => {
  const env = {
    CLOUDFLARE_ACCOUNT_ID: "a".repeat(32),
    CLOUDFLARE_API_TOKEN: "cf-token",
    AGENT_EMAIL_CANARY_FROM: "canary@sender.example",
    AGENT_EMAIL_CANARY_TO: "canary.realm@example.net",
    WITSELF_EMAIL_CANARY_ENDPOINT: "https://cell.example.test",
    WITSELF_EMAIL_CANARY_TOKEN: "witself-token",
  };
  assert.equal(canaryConfiguration(env).timeoutSeconds, 180);
  assert.throws(() => canaryConfiguration({ ...env, WITSELF_EMAIL_CANARY_ENDPOINT: "http://cell.test" }), /HTTPS/);
  assert.throws(() => canaryConfiguration({ ...env, WITSELF_EMAIL_CANARY_ENDPOINT: "https://cell.test?token=bad" }), /HTTPS/);
  assert.throws(() => canaryConfiguration({ ...env, AGENT_EMAIL_CANARY_TO: "bad\naddress" }), /invalid/);
  assert.throws(() => canaryConfiguration({ ...env, AGENT_EMAIL_CANARY_TIMEOUT_SECONDS: "601" }), /between 20 and 600/);
});

test("canary enforces an absolute request deadline", async () => {
  let fetchCalls = 0;
  let clockCalls = 0;
  await assert.rejects(() => runCanary(config, {
    fetch: async () => {
      fetchCalls += 1;
      return Response.json({ success: true, result: { delivered: [config.to], queued: [] } });
    },
    randomUUID: canaryUUIDGenerator(),
    randomCode: () => Number(code),
    now: () => {
      clockCalls += 1;
      return clockCalls === 1 ? 1_000 : 21_001;
    },
  }), /Witself canary request failed/);
  assert.equal(fetchCalls, 0);
});
