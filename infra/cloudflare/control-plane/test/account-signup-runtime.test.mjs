import assert from "node:assert/strict";
import test from "node:test";

import { DurableAccountSignup } from "../src/account-signup-runtime.mjs";
import {
  DurableTargetCellCoordinator,
} from "../src/target-cell-coordinator.mjs";

const PROVISION = "11111111-1111-4111-8111-111111111111";
const ACCOUNT = "acct_signup";
const INVITE = "early-access";
const SIGNUP_IP_SCOPE_PATTERN = /^signup-counter:ip:[0-9a-f]{64}$/;

class Storage {
  constructor() {
    this.values = new Map();
    this.failPhaseOnce = null;
    this.failVerificationResultOnce = false;
  }

  async get(key) {
    const value = this.values.get(key);
    return value === undefined ? undefined : structuredClone(value);
  }

  async put(key, value) {
    if (
      key === "account-signup" &&
      value?.phase === this.failPhaseOnce
    ) {
      this.failPhaseOnce = null;
      throw new Error(`simulated crash before ${value.phase} checkpoint`);
    }
    if (
      key === "account-signup" &&
      value?.verification_email_sent === true &&
      this.failVerificationResultOnce
    ) {
      this.failVerificationResultOnce = false;
      throw new Error("simulated crash after email delivery");
    }
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    this.values.delete(key);
  }

  async list({ prefix = "" } = {}) {
    return new Map(
      [...this.values].filter(([key]) => key.startsWith(prefix)),
    );
  }

  async setAlarm(value) {
    this.alarm = value;
  }

  async deleteAlarm() {
    this.alarm = null;
  }

  async transaction(callback) {
    const staged = new Map(
      [...this.values].map(([key, value]) => [
        key,
        structuredClone(value),
      ]),
    );
    const txn = {
      get: async (key) => {
        const value = staged.get(key);
        return value === undefined ? undefined : structuredClone(value);
      },
      put: async (key, value) => {
        staged.set(key, structuredClone(value));
      },
      delete: async (key) => {
        staged.delete(key);
      },
      list: async ({ prefix = "" } = {}) => new Map(
        [...staged]
          .filter(([key]) => key.startsWith(prefix))
          .map(([key, value]) => [key, structuredClone(value)]),
      ),
    };
    const result = await callback(txn);
    this.values = staged;
    return result;
  }
}

class KV {
  constructor(entries = {}) {
    this.values = new Map(
      Object.entries(entries).map(([key, value]) => [
        key,
        JSON.stringify(value),
      ]),
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

  async delete(key) {
    this.values.delete(key);
  }

  value(key) {
    const value = this.values.get(key);
    return value === undefined ? null : JSON.parse(value);
  }
}

function cell(name, endpoint) {
  return {
    name,
    endpoint,
    cloud: "civo",
    region: "Phoenix",
    region_code: "phx1",
    accepting: true,
    provision_token: `token-${name}`,
    registration_id: `registration-${name}`,
  };
}

class CellService {
  constructor() {
    this.receipts = new Map();
    this.calls = [];
    this.ambiguousFirst = false;
    this.activated = false;
    this.exactRouteUnavailable = false;
    this.omitConsentEcho = false;
    this.mismatchConsentEcho = false;
    this.tokenSequence = 0;
  }

  async fetch(url, init = {}) {
    if (url.endsWith("/v1/version")) {
      return Response.json({
        schema_version: "witself.v0",
        account_provision_protocol: 1,
      });
    }
    if (this.exactRouteUnavailable) {
      return Response.json(
        { error: "not found" },
        { status: 404 },
      );
    }
    const input = JSON.parse(init.body);
    this.calls.push({ url, input });
    let receipt = this.receipts.get(input.provision_id);
    if (!receipt) {
      receipt = {
        request: structuredClone(input),
        account_id: ACCOUNT,
        operator_id: "opr_signup",
      };
      this.receipts.set(input.provision_id, receipt);
      if (this.ambiguousFirst) {
        this.ambiguousFirst = false;
        throw new Error("simulated lost committed response");
      }
    } else if (
      JSON.stringify(receipt.request) !== JSON.stringify(input)
    ) {
      return Response.json(
        { error: "provision_id conflicts with its receipt" },
        { status: 409 },
      );
    }
    if (this.activated) {
      return Response.json(
        { error: "provision receipt is no longer pending" },
        { status: 409 },
      );
    }
    this.tokenSequence++;
    const responseBody = {
      schema_version: "witself.v0",
      provision_id: input.provision_id,
      replayed: this.calls.length > 1,
      account: {
        account_id: receipt.account_id,
        operator_id: receipt.operator_id,
        email: input.email,
        status: "pending",
        bootstrap_token: `bootstrap-${this.tokenSequence}`,
      },
    };
    if (!this.omitConsentEcho) {
      responseBody.recorded_consent_terms_version =
        this.mismatchConsentEcho && input.consent_terms_version != null
          ? `${input.consent_terms_version}-mismatch`
          : input.consent_terms_version ?? null;
      responseBody.recorded_consent_privacy_version =
        input.consent_privacy_version ?? null;
    }
    return Response.json(responseBody, { status: 201 });
  }
}

class TargetAuthority {
  constructor() {
    this.provisions = new Map();
    this.calls = [];
  }

  async request(cellName, path, payload) {
    this.calls.push({ cellName, path, payload: structuredClone(payload) });
    const current = this.provisions.get(payload.provision_id);
    if (path === "/provision/begin") {
      if (current && current.registration_id !== payload.registration_id) {
        throw new Error("provision registration changed");
      }
      this.provisions.set(payload.provision_id, {
        ...(current ?? {}),
        registration_id: payload.registration_id,
      });
      return {
        ok: true,
        provision_id: payload.provision_id,
        registration_id: payload.registration_id,
      };
    }
    if (path === "/provision/attach") {
      assert.ok(current);
      current.account_id = payload.account_id;
      current.route_epoch = payload.route_epoch;
      return {
        ok: true,
        provision_id: payload.provision_id,
        account_id: payload.account_id,
        attached: true,
      };
    }
    if (path === "/provision/promote") {
      assert.equal(current?.account_id, payload.account_id);
      assert.equal(current?.route_epoch, payload.route_epoch);
      current.resident = true;
      current.route_epoch = payload.route_epoch;
      return {
        ok: true,
        provision_id: payload.provision_id,
        account_id: payload.account_id,
        resident: true,
      };
    }
    throw new Error(`unexpected target path ${path}`);
  }
}

function signupRequest(fields = {}) {
  return new Request("https://account-signup.internal/run", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      provision_id: PROVISION,
      email: "Person@Example.com",
      display_name: " Person ",
      invite: INVITE,
      origin: "https://self.witwave.ai",
      ...fields,
    }),
  });
}

test("default signup fetch preserves the platform receiver", async (t) => {
  let receiver = null;
  t.mock.method(globalThis, "fetch", function () {
    receiver = this;
    return Response.json({
      schema_version: "witself.v0",
      account_provision_protocol: 1,
    });
  });
  const runtime = new DurableAccountSignup(
    { id: { name: `provision:${PROVISION}` }, storage: new Storage() },
    {},
  );

  await runtime.requireProvisionProtocol({
    name: "cell-a",
    endpoint: "https://cell-a.example",
  });

  assert.equal(receiver, globalThis);
});

function harness({
  storage = new Storage(),
  directory,
  service = new CellService(),
  target = new TargetAuthority(),
  cells = [
    cell("cell-a", "https://cell-a.example"),
    cell("cell-b", "https://cell-b.example"),
  ],
  sendVerification = async () => false,
  env: envOverrides = {},
  verifyTurnstile,
  consumeCounter,
} = {}) {
  directory ??= new KV(Object.fromEntries(
    cells.map((entry) => [`cell:${entry.name}`, entry]),
  ));
  let placements = 0;
  let inviteReservations = 0;
  const runtime = new DurableAccountSignup(
    { id: { name: `provision:${PROVISION}` }, storage },
    { DIRECTORY: directory, ...envOverrides },
    {
      fetch: (url, init) => service.fetch(url, init),
      placeAccount: async () => ({
        cell: cells[Math.min(placements++, cells.length - 1)],
      }),
      reserveInvite: async () => {
        inviteReservations++;
        return { snapshot: {} };
      },
      targetRequest:
        (cellName, path, payload) =>
          target.request(cellName, path, payload),
      sendVerification,
      verifyTurnstile,
      consumeCounter,
      now: () => new Date("2026-07-25T12:00:00.000Z"),
    },
  );
  return {
    runtime,
    storage,
    directory,
    service,
    target,
    placements: () => placements,
    inviteReservations: () => inviteReservations,
  };
}

function assertNoSignupSecretsOrPII(state) {
  const stored = JSON.stringify(state);
  for (
    const forbidden of [
      "person@example.com",
      "Person",
      INVITE,
      "self.witwave.ai",
      "bootstrap-",
    ]
  ) {
    assert.equal(
      stored.includes(forbidden),
      false,
      `durable signup state must not persist ${forbidden}`,
    );
  }
  assert.equal(Object.hasOwn(state, "request"), false);
  assert.equal(Object.hasOwn(state, "origin"), false);
  assert.equal(Object.hasOwn(state.account ?? {}, "email"), false);
}

test("lost committed cell response replays the same provision on the same cell", async () => {
  const service = new CellService();
  service.ambiguousFirst = true;
  const setup = harness({ service });

  const lost = await setup.runtime.fetch(signupRequest());
  assert.equal(lost.status, 502);
  assert.equal(service.receipts.size, 1);
  assert.equal(
    setup.storage.values.get("account-signup").phase,
    "target_reserved",
  );
  assertNoSignupSecretsOrPII(
    setup.storage.values.get("account-signup"),
  );

  const replay = await setup.runtime.fetch(signupRequest());
  assert.equal(replay.status, 201);
  const result = await replay.json();
  assert.equal(result.replayed, true);
  assert.equal(result.bootstrap_token, "bootstrap-1");
  assert.equal(result.cell.name, "cell-a");
  assert.equal(setup.placements(), 1);
  assert.equal(setup.inviteReservations(), 1);
  assert.equal(
    service.calls.every(({ url }) =>
      url === "https://cell-a.example/v1/accounts:provision-exact"
    ),
    true,
  );
  assert.equal(
    service.calls.every(({ input }) =>
      input.provision_id === PROVISION
    ),
    true,
  );
  assertNoSignupSecretsOrPII(
    setup.storage.values.get("account-signup"),
  );
});

test("route projection crash resumes attach and promotion idempotently", async () => {
  const storage = new Storage();
  storage.failPhaseOnce = "route_projected";
  const setup = harness({ storage });

  const crashed = await setup.runtime.fetch(signupRequest());
  assert.equal(crashed.status, 500);
  assert.equal(
    setup.directory.value(`acct:${ACCOUNT}`).cell,
    "cell-a",
    "the route write committed before the checkpoint crash",
  );
  assert.equal(
    storage.values.get("account-signup").phase,
    "pending_projected",
  );

  const resumed = await setup.runtime.fetch(signupRequest());
  assert.equal(resumed.status, 201);
  assert.equal(
    storage.values.get("account-signup").phase,
    "completed",
  );
  assert.equal(
    setup.target.provisions.get(PROVISION).resident,
    true,
  );
  assert.equal(
    setup.target.calls.filter(({ path }) =>
      path === "/provision/attach"
    ).length,
    1,
  );
  assert.equal(
    setup.target.calls.filter(({ path }) =>
      path === "/provision/promote"
    ).length,
    1,
  );
});

test("signup attach and promotion integrate with exact target-cell authority", async () => {
  const selected = cell("cell-a", "https://cell-a.example");
  const directory = new KV({ "cell:cell-a": selected });
  const targetStorage = new Storage();
  const coordinator = new DurableTargetCellCoordinator(
    { id: { name: "cell-a" }, storage: targetStorage },
    { DIRECTORY: directory },
    {
      now: () => new Date("2026-07-25T12:00:00.000Z"),
      randomUUID: () => "33333333-3333-4333-8333-333333333333",
    },
  );
  const target = {
    request: async (cellName, path, payload) => {
      const response = await coordinator.fetch(
        new Request(`https://target-cell.internal${path}`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            cell_name: cellName,
            ...payload,
          }),
        }),
      );
      const body = await response.json();
      if (!response.ok) {
        const error = new Error(body.error);
        error.status = response.status;
        throw error;
      }
      return body;
    },
  };
  const setup = harness({
    directory,
    cells: [selected],
    target,
  });

  const response = await setup.runtime.fetch(signupRequest());
  assert.equal(response.status, 201);
  assert.equal(
    targetStorage.values.has(`provision:${PROVISION}`),
    false,
  );
  assert.deepEqual(
    targetStorage.values.get(`resident:${ACCOUNT}`),
    {
      account_id: ACCOUNT,
      cell_name: "cell-a",
      registration_id: "registration-cell-a",
      admitted_by: "signup",
      provision_id: PROVISION,
      route_epoch: 0,
      admitted_at: "2026-07-25T12:00:00.000Z",
    },
  );
});

test("conflicting reuse is rejected before any second cell mutation", async () => {
  const setup = harness();
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 201);
  const calls = setup.service.calls.length;

  const conflict = await setup.runtime.fetch(
    signupRequest({ email: "other@example.com" }),
  );
  assert.equal(conflict.status, 409);
  assert.match(
    (await conflict.json()).error,
    /different signup request/,
  );
  assert.equal(setup.service.calls.length, calls);
});

test("old replica exact-route miss is transient and cannot mutate", async () => {
  const service = new CellService();
  service.exactRouteUnavailable = true;
  const setup = harness({ service });

  const unavailable = await setup.runtime.fetch(signupRequest());
  assert.equal(unavailable.status, 502);
  assert.match(
    (await unavailable.json()).error,
    /exact provision route is not available yet/,
  );
  assert.equal(service.receipts.size, 0);
  assert.equal(
    setup.storage.values.get("account-signup").phase,
    "target_reserved",
  );
});

test("a retry cannot re-place the same provision onto another cell", async () => {
  const service = new CellService();
  service.ambiguousFirst = true;
  const setup = harness({ service });
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 502);

  await setup.directory.delete("cell:cell-b");
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 201);
  assert.equal(setup.placements(), 1);
  assert.deepEqual(
    setup.service.calls.map(({ url }) => url),
    [
      "https://cell-a.example/v1/accounts:provision-exact",
      "https://cell-a.example/v1/accounts:provision-exact",
    ],
  );
});

test("completed replay returns a fresh token only while still pending", async () => {
  const setup = harness();
  const initial = await setup.runtime.fetch(signupRequest());
  assert.equal(initial.status, 201);
  assert.equal((await initial.json()).bootstrap_token, "bootstrap-1");

  const replay = await setup.runtime.fetch(signupRequest());
  assert.equal(replay.status, 201);
  assert.equal((await replay.json()).bootstrap_token, "bootstrap-2");

  setup.service.activated = true;
  const activated = await setup.runtime.fetch(signupRequest());
  assert.equal(activated.status, 409);
  assert.equal(
    Object.hasOwn(await activated.json(), "bootstrap_token"),
    false,
  );
});

test("email intent is durable before send so retry cannot duplicate delivery", async () => {
  const storage = new Storage();
  storage.failVerificationResultOnce = true;
  let deliveries = 0;
  const setup = harness({
    storage,
    sendVerification: async () => {
      deliveries++;
      return true;
    },
  });

  assert.equal((await setup.runtime.fetch(signupRequest())).status, 500);
  assert.equal(deliveries, 1);
  assert.equal(
    storage.values.get("account-signup").email_attempted,
    true,
  );
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 201);
  assert.equal(deliveries, 1);
});

test("invite authority consumes one exact use across retries", async () => {
  const storage = new Storage();
  const directory = new KV({
    [`invite:${INVITE}`]: {
      enabled: true,
      max_uses: 1,
      uses: 0,
      created_at: "2026-07-25T00:00:00.000Z",
    },
  });
  const authority = new DurableAccountSignup(
    { id: { name: `invite:${INVITE}` }, storage },
    { DIRECTORY: directory },
    { now: () => new Date("2026-07-25T12:00:00.000Z") },
  );
  const reserve = (provisionID, fingerprint = "a".repeat(64)) =>
    authority.fetch(
      new Request("https://account-signup.internal/invite/reserve", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          invite: INVITE,
          provision_id: provisionID,
          request_fingerprint: fingerprint,
        }),
      }),
    );

  assert.equal((await reserve(PROVISION)).status, 200);
  assert.equal((await reserve(PROVISION)).status, 200);
  assert.equal(directory.value(`invite:${INVITE}`).uses, 1);

  const conflict = await reserve(PROVISION, "b".repeat(64));
  assert.equal(conflict.status, 409);
  const exhausted = await reserve(
    "22222222-2222-4222-8222-222222222222",
  );
  assert.equal(exhausted.status, 403);
  assert.equal(directory.value(`invite:${INVITE}`).uses, 1);
});

test("dark defaults preserve the exact initialized state shape", async () => {
  const storage = new Storage();
  storage.failPhaseOnce = "invite_reserved";
  const setup = harness({ storage });

  assert.equal((await setup.runtime.fetch(signupRequest())).status, 500);
  const state = storage.values.get("account-signup");
  assert.deepEqual(Object.keys(state), [
    "schema_version",
    "revision",
    "phase",
    "provision_id",
    "request_fingerprint",
    "cell",
    "account",
    "created_at",
    "email_attempted",
    "verification_email_sent",
  ]);
  assert.equal(state.phase, "initialized");
  assert.equal(state.revision, 0);
  assert.equal(Object.hasOwn(state, "turnstile_verified"), false);
});

test("source IP and challenge token are optional strings outside the fingerprint", async () => {
  const setup = harness();
  const initial = await setup.runtime.fetch(signupRequest({
    source_ip: "203.0.113.10",
    turnstile_token: "first-secret-token",
  }));
  assert.equal(initial.status, 201);
  const stored = JSON.stringify(setup.storage.values.get("account-signup"));
  assert.equal(stored.includes("203.0.113.10"), false);
  assert.equal(stored.includes("first-secret-token"), false);

  const replay = await setup.runtime.fetch(signupRequest({
    source_ip: "2001:db8::99",
    turnstile_token: "different-secret-token",
  }));
  assert.equal(replay.status, 201);
  assert.equal(setup.placements(), 1);

  for (const fields of [
    { source_ip: 42 },
    { turnstile_token: { token: "wrong shape" } },
  ]) {
    const isolated = harness();
    const response = await isolated.runtime.fetch(signupRequest(fields));
    assert.equal(response.status, 400);
    assert.equal(isolated.storage.values.has("account-signup"), false);
  }
});

test("consent-less canonical fingerprint is byte-stable (golden)", async () => {
  // Dark contract: a signup without consent must keep the exact historical
  // canonical bytes, or every in-flight durable provision would be refused
  // as a different request after a deploy.
  const setup = harness();
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 201);
  assert.equal(
    setup.storage.values.get("account-signup").request_fingerprint,
    "40fe0b8eaf6a593565e96d204616a5d7c4ec4fd64d03b21b5e80de19d10f9656",
  );
  assert.equal(
    Object.hasOwn(setup.service.calls[0].input, "consent_terms_version"),
    false,
  );
});

test("consent versions bind the durable fingerprint and reach the cell", async () => {
  const setup = harness();
  const consent = {
    consent_terms_version: "draft-2026-08-22",
    consent_privacy_version: "draft-2026-08-23",
  };
  const initial = await setup.runtime.fetch(signupRequest(consent));
  assert.equal(initial.status, 201);
  const state = setup.storage.values.get("account-signup");
  assert.notEqual(
    state.request_fingerprint,
    "40fe0b8eaf6a593565e96d204616a5d7c4ec4fd64d03b21b5e80de19d10f9656",
  );
  // Consent lives in the fingerprint and at the cell; the durable state
  // keeps its exact dark shape with no extra fields.
  assert.equal(Object.hasOwn(state, "consent_terms_version"), false);
  assert.equal(Object.hasOwn(state, "consent_privacy_version"), false);
  assert.deepEqual(
    setup.service.calls.map(({ input }) => [
      input.consent_terms_version,
      input.consent_privacy_version,
    ]),
    [["draft-2026-08-22", "draft-2026-08-23"]],
  );

  // Same-consent retry is the ordinary safe replay.
  const replay = await setup.runtime.fetch(signupRequest(consent));
  assert.equal(replay.status, 201);
  assert.equal((await replay.json()).replayed, true);

  // Drifted or dropped consent on retry is a different signup request.
  const calls = setup.service.calls.length;
  for (const drifted of [
    { ...consent, consent_terms_version: "draft-2026-09-01" },
    {},
  ]) {
    const conflict = await setup.runtime.fetch(signupRequest(drifted));
    assert.equal(conflict.status, 409);
    assert.match(
      (await conflict.json()).error,
      /different signup request/,
    );
  }
  assert.equal(setup.service.calls.length, calls);
});

test("consentful provision refuses a cell receipt without consent echoes", async () => {
  const service = new CellService();
  service.omitConsentEcho = true;
  const setup = harness({ service });
  const response = await setup.runtime.fetch(signupRequest({
    consent_terms_version: "draft-2026-08-22",
    consent_privacy_version: "draft-2026-08-23",
  }));

  assert.equal(response.status, 502);
  assert.match(
    (await response.json()).error,
    /did not confirm the requested consent versions/,
  );
  assert.equal(
    setup.storage.values.get("account-signup").phase,
    "target_reserved",
  );
});

test("consentful provision refuses mismatched consent echoes", async () => {
  const service = new CellService();
  service.mismatchConsentEcho = true;
  const setup = harness({ service });
  const response = await setup.runtime.fetch(signupRequest({
    consent_terms_version: "draft-2026-08-22",
    consent_privacy_version: "draft-2026-08-23",
  }));

  assert.equal(response.status, 502);
  assert.match(
    (await response.json()).error,
    /did not confirm the requested consent versions/,
  );
  assert.equal(
    setup.storage.values.get("account-signup").phase,
    "target_reserved",
  );
});

test("malformed consent is rejected before any signup state exists", async () => {
  const versionShapeError =
    "consent versions must be 1 to 64 characters, starting with an alphanumeric and containing only alphanumerics, dots, underscores, or hyphens";
  for (const { fields, error } of [
    {
      fields: { consent_terms_version: "draft-2026-08-22" },
      error:
        "consent_terms_version and consent_privacy_version must be provided together",
    },
    {
      fields: { consent_privacy_version: "draft-2026-08-22" },
      error:
        "consent_terms_version and consent_privacy_version must be provided together",
    },
    {
      fields: { consent_terms_version: 7, consent_privacy_version: "x" },
      error: "consent versions must be strings",
    },
    {
      fields: {
        consent_terms_version: "a".repeat(65),
        consent_privacy_version: "draft-2026-08-22",
      },
      error: versionShapeError,
    },
    {
      fields: {
        consent_terms_version: "person@example.com",
        consent_privacy_version: "draft-2026-08-22",
      },
      error: versionShapeError,
    },
    {
      fields: { consent_terms_version: "   ", consent_privacy_version: "x" },
      error: versionShapeError,
    },
    {
      fields: {
        consent_terms_version: "draft\u0007bell",
        consent_privacy_version: "x",
      },
      error: versionShapeError,
    },
  ]) {
    const isolated = harness();
    const response = await isolated.runtime.fetch(signupRequest(fields));
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error, error);
    assert.equal(isolated.storage.values.has("account-signup"), false);
  }
});

test("invalid Turnstile requests return the safe challenge URL before invite use", async () => {
  let verifications = 0;
  const setup = harness({
    env: {
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
    },
    verifyTurnstile: async (input) => {
      verifications++;
      assert.deepEqual(input, {
        secretKey: "server-secret",
        token: "bad-token",
        remoteIp: "203.0.113.11",
      });
      return { ok: false, reason: "invalid" };
    },
  });

  const response = await setup.runtime.fetch(signupRequest({
    source_ip: "203.0.113.11",
    turnstile_token: "bad-token",
  }));
  assert.equal(response.status, 403);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "turnstile challenge required",
    challenge_url: "https://self.witwave.ai/signup/challenge",
  });
  assert.equal(verifications, 1);
  assert.equal(setup.inviteReservations(), 0);
  assert.equal(setup.storage.values.has("account-signup"), false);
});

test("Turnstile outages pause signup with an explicit retryable response", async () => {
  const setup = harness({
    env: {
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
    },
    verifyTurnstile: async () => ({
      ok: false,
      reason: "unavailable",
    }),
  });
  const response = await setup.runtime.fetch(signupRequest({
    turnstile_token: "token",
  }));
  assert.equal(response.status, 503);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "turnstile verification unavailable",
    retryable: true,
  });
  assert.equal(setup.inviteReservations(), 0);
  assert.equal(setup.storage.values.has("account-signup"), false);
});

test("an ambiguous counter outcome replays its marker without re-verifying Turnstile", async () => {
  const counterRuntimes = new Map();
  const counterCalls = [];
  let loseGlobalResponse = true;
  let namespace;
  namespace = {
    idFromName: (name) => ({ name }),
    get: (id) => ({
      fetch: async (request) => {
        let runtime = counterRuntimes.get(id.name);
        if (!runtime) {
          runtime = new DurableAccountSignup(
            { id, storage: new Storage() },
            { ACCOUNT_SIGNUP: namespace },
            { now: () => new Date("2026-07-25T12:00:00.000Z") },
          );
          counterRuntimes.set(id.name, runtime);
        }
        const response = await runtime.fetch(request);
        const body = await response.clone().json();
        counterCalls.push({ scope: id.name, ...body });
        if (id.name === "signup-counter:global" && loseGlobalResponse) {
          loseGlobalResponse = false;
          throw new Error("simulated lost committed counter response");
        }
        return response;
      },
    }),
  };

  let verifications = 0;
  const setup = harness({
    env: {
      ACCOUNT_SIGNUP: namespace,
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
      CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
    },
    verifyTurnstile: async () => {
      verifications++;
      return { ok: true };
    },
  });
  const request = (sourceIP) => signupRequest({
    source_ip: sourceIP,
    turnstile_token: "one-time-token",
  });

  const ambiguous = await setup.runtime.fetch(request("203.0.113.12"));
  assert.equal(ambiguous.status, 502);
  assert.match((await ambiguous.json()).error, /counter outcome is ambiguous/);
  assert.equal(verifications, 1);
  assert.deepEqual(
    setup.storage.values.get("account-signup").phase,
    "abuse_preflight",
  );

  const replay = await setup.runtime.fetch(request("198.51.100.44"));
  assert.equal(replay.status, 201);
  assert.equal(verifications, 1);
  assert.deepEqual(
    counterCalls.map(({ scope, replayed }) => ({ scope, replayed })),
    [
      { scope: counterCalls[0].scope, replayed: false },
      { scope: "signup-counter:global", replayed: false },
      { scope: counterCalls[0].scope, replayed: true },
      { scope: "signup-counter:global", replayed: true },
    ],
  );
  assert.match(counterCalls[0].scope, /^signup-counter:ip:[0-9a-f]{64}$/);
  assert.equal(
    setup.storage.values.get("account-signup").turnstile_verified,
    true,
  );
  assert.equal(
    Object.hasOwn(
      setup.storage.values.get("account-signup"),
      "signup_ip_scope",
    ),
    false,
  );

  const completedReplay = await setup.runtime.fetch(signupRequest({
    source_ip: "198.51.100.44",
    turnstile_token: "replacement-token",
  }));
  assert.equal(completedReplay.status, 201);
  assert.equal(verifications, 1);
  assert.equal(counterCalls.length, 4);
});

test("counter-only ambiguous retry keeps its hashed IP scope across networks", async () => {
  const markers = new Map();
  const calls = [];
  let loseFirstResponse = true;
  const setup = harness({
    env: {
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
      CP_SIGNUP_DAILY_LIMIT_GLOBAL: "0",
    },
    consumeCounter: async (input) => {
      calls.push(structuredClone(input));
      const existing = markers.get(input.scope);
      if (existing) return { ...existing, replayed: true };
      const verdict = { allowed: true, count: 1, replayed: false };
      markers.set(input.scope, verdict);
      if (loseFirstResponse) {
        loseFirstResponse = false;
        throw new Error("simulated lost committed counter response");
      }
      return verdict;
    },
  });

  const first = await setup.runtime.fetch(signupRequest({
    source_ip: "203.0.113.21",
  }));
  assert.equal(first.status, 500);
  const checkpoint = setup.storage.values.get("account-signup");
  assert.equal(checkpoint.phase, "abuse_preflight");
  assert.match(checkpoint.signup_ip_scope, SIGNUP_IP_SCOPE_PATTERN);
  assert.equal(Object.hasOwn(checkpoint, "turnstile_verified"), false);

  const retry = await setup.runtime.fetch(signupRequest({
    source_ip: "198.51.100.21",
  }));
  assert.equal(retry.status, 201);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].scope, calls[0].scope);
  assert.equal(markers.size, 1);
  assert.equal(
    Object.hasOwn(
      setup.storage.values.get("account-signup"),
      "signup_ip_scope",
    ),
    false,
  );
});

test("a definitive counter denial deletes preflight state and logs only scope", async (t) => {
  const logs = [];
  t.mock.method(console, "log", (...values) => logs.push(values.join(" ")));
  const calls = [];
  const setup = harness({
    env: {
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
      CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
    },
    verifyTurnstile: async () => ({ ok: true }),
    consumeCounter: async (input) => {
      calls.push(input);
      return { allowed: input.scope !== "signup-counter:global" };
    },
  });
  const response = await setup.runtime.fetch(signupRequest({
    source_ip: "203.0.113.13",
    turnstile_token: "must-never-be-logged",
  }));
  assert.equal(response.status, 429);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "signup rate limit exceeded",
  });
  assert.equal(calls.length, 2, "IP is consumed before the global counter");
  assert.equal(setup.storage.values.has("account-signup"), false);
  assert.equal(setup.inviteReservations(), 0);
  assert.deepEqual(logs, [
    "signup: daily counter denied scope signup-counter:global",
  ]);
  assert.equal(logs[0].includes(PROVISION), false);
  assert.equal(logs[0].includes("203.0.113.13"), false);
  assert.equal(logs[0].includes("must-never-be-logged"), false);
});

test("counter consume is accepted only by the signup-counter role", async () => {
  const input = (provisionID) => new Request(
    "https://account-signup.internal/counter/consume",
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ provision_id: provisionID, limit: 1 }),
    },
  );
  const wrongRole = new DurableAccountSignup(
    {
      id: { name: `provision:${PROVISION}` },
      storage: new Storage(),
    },
    {},
  );
  assert.equal((await wrongRole.fetch(input("counter-a"))).status, 400);

  const storage = new Storage();
  const authority = new DurableAccountSignup(
    { id: { name: "signup-counter:global" }, storage },
    {},
    { now: () => new Date("2026-07-25T12:00:00.000Z") },
  );
  const allowed = await authority.fetch(input("counter-a"));
  assert.equal(allowed.status, 200);
  assert.deepEqual(await allowed.json(), {
    ok: true,
    scope: "signup-counter:global",
    provision_id: "counter-a",
    allowed: true,
    count: 1,
    limit: 1,
    day: "2026-07-25",
    replayed: false,
  });
  const replay = await authority.fetch(input("counter-a"));
  assert.equal((await replay.json()).replayed, true);
  const denied = await authority.fetch(input("counter-b"));
  assert.equal((await denied.json()).allowed, false);
  assert.equal(storage.values.has("account-signup"), false);
});
