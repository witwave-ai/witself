import assert from "node:assert/strict";
import { timingSafeEqual as nodeTimingSafeEqual } from "node:crypto";
import { register } from "node:module";
import test from "node:test";

register(new URL("./fixtures/cloudflare-containers-loader.mjs", import.meta.url));
const worker = (await import("../src/index.js")).default;

if (typeof crypto.subtle.timingSafeEqual !== "function") {
  Object.defineProperty(Object.getPrototypeOf(crypto.subtle), "timingSafeEqual", {
    configurable: true,
    value(a, b) {
      const ab = new Uint8Array(a);
      const bb = new Uint8Array(b);
      if (ab.byteLength !== bb.byteLength) return false;
      return nodeTimingSafeEqual(ab, bb);
    },
  });
}

const ORIGIN = "https://cp.test.invalid";
const FLEET_TOKEN = "fleet-test-token";
const AUTH = { Authorization: `Bearer ${FLEET_TOKEN}` };

class KVFake {
  constructor(entries = {}) {
    this.map = new Map(
      Object.entries(entries).map(([key, value]) => [
        key,
        JSON.stringify(value),
      ]),
    );
  }

  async get(key, options) {
    const value = this.map.get(key);
    if (value === undefined) return null;
    return options?.type === "json" ? JSON.parse(value) : value;
  }

  async put(key, value) {
    this.map.set(key, value);
  }

  async delete(key) {
    this.map.delete(key);
  }

  async list({ prefix = "", cursor } = {}) {
    const keys = [...this.map.keys()]
      .filter((key) => key.startsWith(prefix))
      .sort();
    const start = cursor == null
      ? 0
      : keys.findIndex((key) => key > cursor);
    const offset = start === -1 ? keys.length : start;
    const page = keys.slice(offset, offset + 2);
    const listComplete = offset + page.length >= keys.length;
    return {
      keys: page.map((name) => ({ name })),
      list_complete: listComplete,
      cursor: listComplete ? undefined : page.at(-1),
    };
  }

  value(key) {
    const value = this.map.get(key);
    return value === undefined ? null : JSON.parse(value);
  }
}

class InviteAuthorityFake {
  constructor() {
    this.states = new Map();
    this.useRecords = new Map();
    this.calls = [];
  }

  binding() {
    return {
      idFromName: (name) => ({ name }),
      get: (id) => ({
        fetch: async (request) => {
          this.calls.push({ name: id.name, request });
          if (
            request.method !== "GET" ||
            new URL(request.url).pathname !== "/invite/status"
          ) {
            return Response.json({ error: "unexpected authority request" }, {
              status: 500,
            });
          }
          const state = this.states.get(id.name);
          if (state == null) {
            return Response.json({
              schema_version: "witself.v0",
              initialized: false,
            });
          }
          return Response.json({
            schema_version: "witself.v0",
            initialized: true,
            generation: state.generation,
            uses: state.uses,
          });
        },
      }),
    };
  }

  set(code, generation, uses) {
    this.states.set(`invite:${code}`, { generation, uses });
  }
}

function makeEnv(entries = {}, options = {}) {
  const kv = new KVFake(entries);
  const authority = options.authority ?? new InviteAuthorityFake();
  const env = {
    DIRECTORY: kv,
    FLEET_TOKEN,
    ...(options.withoutAuthority
      ? {}
      : { ACCOUNT_SIGNUP: authority.binding() }),
  };
  return { env, kv, authority };
}

async function run(env, path, init = {}) {
  return worker.fetch(
    new Request(`${ORIGIN}${path}`, init),
    env,
    { waitUntil() {} },
  );
}

function post(env, body) {
  return run(env, "/v1/invites", {
    method: "POST",
    headers: { ...AUTH, "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

test("invite list, show, disable, enable, and delete round-trip", async () => {
  const { env, kv, authority } = makeEnv();
  const code = "launch-2026";
  const notBefore = new Date(Date.now() - 60_000).toISOString();
  const expiresAt = new Date(Date.now() + 3_600_000).toISOString();
  const created = await post(env, {
    code,
    enabled: true,
    max_uses: 2,
    not_before: notBefore,
    expires_at: expiresAt,
    cell: "cell-a",
    region: "us-west-2",
    note: "launch operators",
  });
  assert.equal(created.status, 201);
  const createdInvite = (await created.json()).invite;
  assert.equal(createdInvite.code, code);
  assert.equal(createdInvite.valid, true);
  assert.equal(createdInvite.uses, 0);

  const initialList = await run(env, "/v1/invites", { headers: AUTH });
  assert.equal(initialList.status, 200);
  assert.deepEqual(
    (await initialList.json()).invites.map((invite) => invite.code),
    [code],
  );

  authority.set(code, createdInvite.created_at, 2);
  authority.useRecords.set(`${code}:provision-a`, { reserved: true });

  const disabled = await post(env, { code, enabled: false });
  assert.equal(disabled.status, 200);
  const disabledInvite = (await disabled.json()).invite;
  assert.equal(disabledInvite.enabled, false);
  assert.equal(disabledInvite.reason, "disabled");
  assert.deepEqual(
    {
      not_before: disabledInvite.not_before,
      expires_at: disabledInvite.expires_at,
      max_uses: disabledInvite.max_uses,
      cell: disabledInvite.cell,
      region: disabledInvite.region,
      note: disabledInvite.note,
      uses: disabledInvite.uses,
      created_at: disabledInvite.created_at,
    },
    {
      not_before: notBefore,
      expires_at: expiresAt,
      max_uses: 2,
      cell: "cell-a",
      region: "us-west-2",
      note: "launch operators",
      uses: 0,
      created_at: createdInvite.created_at,
    },
  );

  const disabledList = await run(env, "/v1/invites", { headers: AUTH });
  const listed = (await disabledList.json()).invites[0];
  assert.equal(listed.enabled, false);
  assert.equal(listed.reason, "disabled");

  const shownDisabled = await run(env, `/v1/invites/${code}`, {
    headers: AUTH,
  });
  assert.equal(shownDisabled.status, 200);
  const liveDisabled = (await shownDisabled.json()).invite;
  assert.equal(liveDisabled.uses, 2);
  assert.equal(liveDisabled.enabled, false);
  assert.equal(liveDisabled.exhausted, true);
  assert.equal(liveDisabled.reason, "disabled");

  const enabled = await post(env, { code, enabled: true });
  assert.equal(enabled.status, 200);
  assert.equal((await enabled.json()).invite.enabled, true);
  const shownEnabled = await run(env, `/v1/invites/${code}`, {
    headers: AUTH,
  });
  const liveEnabled = (await shownEnabled.json()).invite;
  assert.equal(liveEnabled.uses, 2);
  assert.equal(liveEnabled.valid, false);
  assert.equal(liveEnabled.exhausted, true);
  assert.equal(liveEnabled.reason, "fully used");

  const deleted = await run(env, `/v1/invites/${code}`, {
    method: "DELETE",
    headers: AUTH,
  });
  assert.equal(deleted.status, 200);
  assert.deepEqual(await deleted.json(), {
    schema_version: "witself.v0",
    deleted: true,
  });
  assert.equal(kv.value(`invite:${code}`), null);
  assert.deepEqual(authority.useRecords.get(`${code}:provision-a`), {
    reserved: true,
  });

  const authorityCalls = authority.calls.length;
  const missing = await run(env, `/v1/invites/${code}`, { headers: AUTH });
  assert.equal(missing.status, 404);
  assert.equal((await missing.json()).error, "unknown invite");
  assert.equal(authority.calls.length, authorityCalls);

  const deletedAgain = await run(env, `/v1/invites/${code}`, {
    method: "DELETE",
    headers: AUTH,
  });
  assert.equal(deletedAgain.status, 200);
  assert.deepEqual(await deletedAgain.json(), {
    schema_version: "witself.v0",
    deleted: false,
  });
});

test("list derives independent verdict flags with the signup rules", async () => {
  const { env } = makeEnv({
    "invite:disabled": {
      enabled: false,
      expires_at: "2000-01-01T00:00:00.000Z",
      max_uses: 1,
      uses: 1,
    },
    "invite:exhausted": { enabled: true, max_uses: 2, uses: 2 },
    "invite:expired": {
      enabled: true,
      expires_at: "2000-01-01T00:00:00.000Z",
      uses: 0,
    },
    "invite:future": {
      enabled: true,
      not_before: "2999-01-01T00:00:00.000Z",
      uses: 0,
    },
    "invite:valid": { uses: 0 },
  });

  const response = await run(env, "/v1/invites", { headers: AUTH });
  assert.equal(response.status, 200);
  const invites = new Map(
    (await response.json()).invites.map((invite) => [invite.code, invite]),
  );
  assert.deepEqual(
    {
      enabled: invites.get("disabled").enabled,
      exhausted: invites.get("disabled").exhausted,
      expired: invites.get("disabled").expired,
      not_yet_valid: invites.get("disabled").not_yet_valid,
      valid: invites.get("disabled").valid,
      reason: invites.get("disabled").reason,
    },
    {
      enabled: false,
      exhausted: true,
      expired: true,
      not_yet_valid: false,
      valid: false,
      reason: "disabled",
    },
  );
  assert.equal(invites.get("exhausted").reason, "fully used");
  assert.equal(invites.get("exhausted").exhausted, true);
  assert.equal(invites.get("expired").reason, "expired");
  assert.equal(invites.get("expired").expired, true);
  assert.equal(invites.get("future").reason, "not yet valid");
  assert.equal(invites.get("future").not_yet_valid, true);
  assert.equal(invites.get("valid").enabled, true);
  assert.equal(invites.get("valid").valid, true);
});

test("show rebases a recreated generation and fails closed on bad authority state", async () => {
  const code = "launch-status";
  const entry = {
    enabled: true,
    uses: 1,
    created_at: "2026-08-28T00:00:00.000Z",
  };
  const unavailable = makeEnv(
    { [`invite:${code}`]: entry },
    { withoutAuthority: true },
  );
  const noAuthority = await run(
    unavailable.env,
    `/v1/invites/${code}`,
    { headers: AUTH },
  );
  assert.equal(noAuthority.status, 503);
  assert.deepEqual(await noAuthority.json(), {
    schema_version: "witself.v0",
    error: "invite status unavailable",
  });

  const mismatched = makeEnv({ [`invite:${code}`]: entry });
  mismatched.authority.set(code, "a-different-generation", 9);
  const priorGeneration = await run(
    mismatched.env,
    `/v1/invites/${code}`,
    { headers: AUTH },
  );
  assert.equal(priorGeneration.status, 200);
  assert.equal((await priorGeneration.json()).invite.uses, 1);

  mismatched.authority.set(code, entry.created_at, -1);
  const invalidState = await run(
    mismatched.env,
    `/v1/invites/${code}`,
    { headers: AUTH },
  );
  assert.equal(invalidState.status, 503);
  const errorBody = await invalidState.json();
  assert.equal(errorBody.error, "invite status unavailable");
  assert.equal(JSON.stringify(errorBody).includes(code), false);
});

test("a deleted code can be recreated while prior authority records remain", async () => {
  const code = "launch-reused";
  const oldGeneration = "2026-01-01T00:00:00.000Z";
  const { env, authority } = makeEnv({
    [`invite:${code}`]: {
      enabled: true,
      max_uses: 1,
      uses: 1,
      created_at: oldGeneration,
    },
  });
  authority.set(code, oldGeneration, 1);
  authority.useRecords.set(`${code}:old-provision`, { reserved: true });

  const removed = await run(env, `/v1/invites/${code}`, {
    method: "DELETE",
    headers: AUTH,
  });
  assert.equal((await removed.json()).deleted, true);
  const recreated = await post(env, { code, max_uses: 2 });
  assert.equal(recreated.status, 201);
  const recreatedInvite = (await recreated.json()).invite;
  assert.notEqual(recreatedInvite.created_at, oldGeneration);
  assert.equal(recreatedInvite.uses, 0);

  const shown = await run(env, `/v1/invites/${code}`, { headers: AUTH });
  assert.equal(shown.status, 200);
  const live = (await shown.json()).invite;
  assert.equal(live.uses, 0);
  assert.equal(live.valid, true);
  assert.deepEqual(authority.useRecords.get(`${code}:old-provision`), {
    reserved: true,
  });
});

test("POST rejects malformed invite shapes without echoing a code", async () => {
  const { env, kv } = makeEnv();
  const code = "launch-validation";
  const cases = [
    { raw: "{" },
    { body: null },
    { body: [] },
    { body: { code: 123 } },
    { body: { code: null } },
    { body: { code, enabled: "false" } },
    { body: { code, enabled: null } },
    { body: { code, not_before: 123 } },
    { body: { code, expires_at: "not-a-time" } },
    { body: { code, max_uses: 0 } },
    { body: { code, max_uses: 1.5 } },
    { body: { code, max_uses: Number.MAX_SAFE_INTEGER + 1 } },
    { body: { code, cell: 7 } },
    { body: { code, cell: "Cell_A" } },
    { body: { code, region: {} } },
    { body: { code, region: "US West" } },
    { body: { code, note: 7 } },
    { body: { code, note: "x".repeat(201) } },
    {
      body: {
        code,
        not_before: "2027-01-01T00:00:00.000Z",
        expires_at: "2026-01-01T00:00:00.000Z",
      },
    },
  ];

  for (const [index, item] of cases.entries()) {
    const response = await run(env, "/v1/invites", {
      method: "POST",
      headers: { ...AUTH, "Content-Type": "application/json" },
      body: item.raw ?? JSON.stringify(item.body),
    });
    assert.equal(response.status, 400, `case ${index}`);
    const text = await response.text();
    assert.equal(text.includes(code), false, `case ${index}`);
  }
  assert.deepEqual(
    [...kv.map.keys()].filter((key) => key.startsWith("invite:")),
    [],
  );
});

test("POST can generate a code and explicit window nulls clear an upsert", async () => {
  const { env, kv } = makeEnv();
  const generated = await post(env, { max_uses: 3, note: "generated" });
  assert.equal(generated.status, 201);
  const generatedInvite = (await generated.json()).invite;
  assert.match(generatedInvite.code, /^[a-z0-9][a-z0-9-]{2,63}$/);

  const code = "clear-window";
  const created = await post(env, {
    code,
    not_before: "2026-01-01T00:00:00.000Z",
    expires_at: "2027-01-01T00:00:00.000Z",
    max_uses: 4,
    cell: "cell-a",
    note: "keep me",
  });
  assert.equal(created.status, 201);
  const cleared = await post(env, {
    code,
    not_before: null,
    expires_at: null,
    max_uses: null,
    cell: null,
    note: null,
  });
  assert.equal(cleared.status, 200);
  const value = kv.value(`invite:${code}`);
  assert.equal(value.not_before, null);
  assert.equal(value.expires_at, null);
  assert.equal(value.max_uses, null);
  assert.equal(value.cell, "cell-a");
  assert.equal(value.note, "keep me");
});

test("invite routes remain fleet-token gated", async () => {
  const { env } = makeEnv();
  for (const [path, method] of [
    ["/v1/invites", "GET"],
    ["/v1/invites", "POST"],
    ["/v1/invites/launch-auth", "GET"],
    ["/v1/invites/launch-auth", "DELETE"],
  ]) {
    const response = await run(env, path, {
      method,
      ...(method === "POST" ? { body: "{}" } : {}),
    });
    assert.equal(response.status, 401, `${method} ${path}`);
  }
});
