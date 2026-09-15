import assert from "node:assert/strict";
import test from "node:test";

import {
  DurableAccountSignup,
  inviteVerdict,
} from "../src/account-signup-runtime.mjs";
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

test("invite authority failures never echo the invite code", async () => {
  const attempt = (binding) => new DurableAccountSignup(
    { id: { name: `provision:${PROVISION}` }, storage: new Storage() },
    { ACCOUNT_SIGNUP: binding },
    { now: () => new Date("2026-07-25T12:00:00.000Z") },
  ).fetch(signupRequest());

  const throwing = await attempt({
    idFromName(name) {
      throw new Error(`binding failed for ${name}`);
    },
  });
  assert.equal(throwing.status, 502);
  const throwingText = await throwing.text();
  assert.equal(throwingText.includes(INVITE), false);
  assert.equal(throwingText.includes("invite:"), false);
  assert.equal(
    JSON.parse(throwingText).error,
    "invite reservation outcome is ambiguous",
  );

  const providerFailure = await attempt({
    idFromName: (name) => ({ name }),
    get: (id) => ({
      fetch: async () => new Response(
        `internal failure for ${id.name}`,
        { status: 500 },
      ),
    }),
  });
  assert.equal(providerFailure.status, 500);
  const providerText = await providerFailure.text();
  assert.equal(providerText.includes(INVITE), false);
  assert.equal(providerText.includes("invite:"), false);
  assert.equal(
    JSON.parse(providerText).error,
    "invite reservation failed (HTTP 500)",
  );
});

function harness({
  provisionID = PROVISION,
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
    { id: { name: `provision:${provisionID}` }, storage },
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

function historicalInitializedCheckpoint() {
  return {
    schema_version: "witself.signup.v1",
    revision: 0,
    phase: "initialized",
    provision_id: PROVISION,
    request_fingerprint:
      "40fe0b8eaf6a593565e96d204616a5d7c4ec4fd64d03b21b5e80de19d10f9656",
    cell: null,
    account: null,
    created_at: "2026-07-25T12:00:00.000Z",
    email_attempted: false,
    verification_email_sent: false,
  };
}

test("malformed stored signup envelopes fail closed without side effects", async (t) => {
  const initial = historicalInitializedCheckpoint();
  const cases = [
    ["stored null", null],
    ["stored false", false],
    ["stored zero", 0],
    ["stored empty string", ""],
    ["stored string", "checkpoint-value-must-not-escape"],
    ["stored array", []],
    ["missing schema", { ...initial, schema_version: undefined }],
    ["unknown schema", { ...initial, schema_version: "witself.signup.v2" }],
    ["malformed schema", { ...initial, schema_version: ["witself.signup.v1"] }],
    ["missing phase", { ...initial, phase: undefined }],
    ["unknown phase", { ...initial, phase: "legal_rejected" }],
    ["inherited constructor phase", { ...initial, phase: "constructor" }],
    ["inherited toString phase", { ...initial, phase: "toString" }],
    ["inherited prototype phase", { ...initial, phase: "__proto__" }],
    ["numeric phase", { ...initial, phase: 0 }],
    ["array phase", { ...initial, phase: ["initialized"] }],
    ["missing revision", { ...initial, revision: undefined }],
    ["null revision", { ...initial, revision: null }],
    ["string revision", { ...initial, revision: "0" }],
    ["negative revision", { ...initial, revision: -1 }],
    ["fractional revision", { ...initial, revision: 0.5 }],
    ["unsafe revision", { ...initial, revision: Number.MAX_SAFE_INTEGER + 1 }],
    ["nonfinite revision", { ...initial, revision: Infinity }],
    ["NaN revision", { ...initial, revision: NaN }],
    ["unknown schema before fingerprint conflict", {
      ...initial,
      schema_version: "witself.signup.v2",
      request_fingerprint: "different-request-fingerprint",
    }],
  ];
  for (const [name, state] of cases) {
    await t.test(name, async () => {
      const storage = new Storage();
      await storage.put("account-signup", state);
      const before = structuredClone(storage.values);
      const callbacks = [];
      const setup = harness({
        storage,
        env: {
          CP_SIGNUP_TURNSTILE_ENABLED: "true",
          CP_SIGNUP_TURNSTILE_SECRET_KEY: "fixture-secret",
          CP_SIGNUP_DAILY_LIMIT_PER_IP: "1",
          CP_SIGNUP_DAILY_LIMIT_GLOBAL: "1",
        },
        verifyTurnstile: async () => {
          callbacks.push("turnstile");
          return { ok: true };
        },
        consumeCounter: async () => {
          callbacks.push("counter");
          return { allowed: true };
        },
        sendVerification: async () => {
          callbacks.push("email");
          return false;
        },
      });
      const directoryBefore = structuredClone(setup.directory.values);

      const response = await setup.runtime.fetch(signupRequest());

      assert.equal(response.status, 500);
      assert.deepEqual(await response.json(), {
        schema_version: "witself.v0",
        error: "account signup checkpoint is invalid",
      });
      assert.deepEqual(callbacks, []);
      assert.equal(setup.inviteReservations(), 0);
      assert.equal(setup.placements(), 0);
      assert.deepEqual(setup.target.calls, []);
      assert.deepEqual(setup.service.calls, []);
      assert.equal(setup.service.receipts.size, 0);
      assert.deepEqual(storage.values, before);
      assert.deepEqual(setup.directory.values, directoryBefore);
    });
  }
});

test("an absent signup checkpoint still creates one exact account", async () => {
  const setup = harness();
  assert.equal(await setup.storage.get("account-signup"), undefined);
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 201);
  assert.equal(setup.service.receipts.size, 1);
  assert.equal(setup.storage.values.get("account-signup").phase, "completed");
});

test("historical initialized signup retains its exact fingerprint and revision", async () => {
  const storage = new Storage();
  const state = historicalInitializedCheckpoint();
  await storage.put("account-signup", state);
  storage.failPhaseOnce = "invite_reserved";
  const setup = harness({ storage });

  assert.equal((await setup.runtime.fetch(signupRequest())).status, 500);
  assert.equal(setup.inviteReservations(), 1);
  assert.deepEqual(storage.values.get("account-signup"), state);
  assert.equal((await setup.runtime.fetch(signupRequest())).status, 201);
  assert.equal(setup.service.receipts.size, 1);
});

test("every historical signup phase resumes after its committed checkpoint", async (t) => {
  const phases = [
    "abuse_preflight",
    "initialized",
    "invite_reserved",
    "cell_selected",
    "protocol_verified",
    "target_reserved",
    "cell_acknowledged",
    "target_attached",
    "pending_projected",
    "route_projected",
    "resident_promoted",
    "completed",
  ];
  for (const mode of ["dark", "counters", "challenge-and-counters"]) {
    for (const phase of phases) {
      if (mode === "dark" && phase === "abuse_preflight") continue;
      await t.test(`${mode}: ${phase}`, async () => {
        class CrashAfterCheckpointStorage extends Storage {
          async put(key, value) {
            await super.put(key, value);
            if (key === "account-signup" && value.phase === this.stopPhase) {
              this.stopPhase = null;
              throw new Error("simulated crash after committed checkpoint");
            }
          }
        }
        const storage = new CrashAfterCheckpointStorage();
        storage.stopPhase = phase;
        let verifications = 0;
        let counterCalls = 0;
        let emails = 0;
        const options = {
          storage,
          env: mode === "dark" ? {} : {
            CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
            CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
            ...(mode === "challenge-and-counters" ? {
              CP_SIGNUP_TURNSTILE_ENABLED: "true",
              CP_SIGNUP_TURNSTILE_SECRET_KEY: "fixture-secret",
            } : {}),
          },
          verifyTurnstile: async () => {
            verifications++;
            return { ok: true };
          },
          consumeCounter: async () => {
            counterCalls++;
            return { allowed: true };
          },
          sendVerification: async () => {
            emails++;
            return true;
          },
        };
        const setup = harness(options);
        const first = await setup.runtime.fetch(signupRequest());
        assert.equal(first.status, 500);
        const checkpoint = await storage.get("account-signup");
        assert.equal(checkpoint.phase, phase);
        assert.equal(
          Object.hasOwn(checkpoint, "turnstile_verified"),
          mode === "challenge-and-counters",
        );
        assert.equal(
          Object.hasOwn(checkpoint, "signup_ip_scope"),
          phase === "abuse_preflight",
        );

        // A new runtime reads only the durable checkpoint; external receipts
        // retain exactly the side effects completed before the simulated crash.
        const resumed = harness({
          ...options,
          directory: setup.directory,
          service: setup.service,
          target: setup.target,
        });
        assert.equal((await resumed.runtime.fetch(signupRequest())).status, 201);
        const completed = await storage.get("account-signup");
        assert.equal(completed.phase, "completed");
        assert.equal(completed.provision_id, checkpoint.provision_id);
        assert.equal(completed.request_fingerprint, checkpoint.request_fingerprint);
        assert.equal(completed.verification_email_sent, true);
        assert.equal(setup.placements() + resumed.placements(), 1);
        assert.equal(setup.inviteReservations() + resumed.inviteReservations(), 1);
        assert.equal(setup.service.receipts.size, 1);
        assert.equal(verifications, mode === "challenge-and-counters" ? 1 : 0);
        assert.equal(counterCalls, mode === "dark" ? 0 : 2);
        assert.equal(emails, 1);

        // Completed states with persisted email intent/results also remain
        // exact replays, without another email, quota use, or state write.
        assert.equal((await resumed.runtime.fetch(signupRequest())).status, 201);
        assert.deepEqual(await storage.get("account-signup"), completed);
        assert.equal(emails, 1);
        assert.equal(counterCalls, mode === "dark" ? 0 : 2);
      });
    }
  }
});

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

test("replaying an earlier reservation cannot regress the live projection", async () => {
  const generation = "2026-07-25T00:00:00.000Z";
  const storage = new Storage();
  const directory = new KV({
    [`invite:${INVITE}`]: {
      enabled: true,
      max_uses: 3,
      uses: 0,
      created_at: generation,
    },
  });
  const authority = new DurableAccountSignup(
    { id: { name: `invite:${INVITE}` }, storage },
    { DIRECTORY: directory },
    { now: () => new Date("2026-07-25T12:00:00.000Z") },
  );
  const fingerprint = "a".repeat(64);
  const reserve = (provisionID) => authority.fetch(
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
  const secondProvision = "22222222-2222-4222-8222-222222222222";

  assert.equal((await reserve(PROVISION)).status, 200);
  assert.equal((await reserve(secondProvision)).status, 200);
  assert.equal(directory.value(`invite:${INVITE}`).uses, 2);
  const replay = await reserve(PROVISION);
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).uses, 2);
  assert.equal(directory.value(`invite:${INVITE}`).uses, 2);
});

test("legacy invite count states keep unversioned retry keys", async () => {
  const generation = "2026-07-25T00:00:00.000Z";
  const fingerprint = "a".repeat(64);
  const storage = new Storage();
  await storage.put("invite-count", { generation, count: 1 });
  await storage.put(`invite-use:${PROVISION}`, {
    provision_id: PROVISION,
    request_fingerprint: fingerprint,
    snapshot: {},
    reserved_at: "2026-07-25T01:00:00.000Z",
  });
  const directory = new KV({
    [`invite:${INVITE}`]: {
      enabled: true,
      max_uses: 3,
      uses: 1,
      created_at: generation,
    },
  });
  const authority = new DurableAccountSignup(
    { id: { name: `invite:${INVITE}` }, storage },
    { DIRECTORY: directory },
    { now: () => new Date("2026-07-25T12:00:00.000Z") },
  );
  const reserve = (provisionID) => authority.fetch(
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
  const nextProvision = "22222222-2222-4222-8222-222222222222";
  assert.equal((await reserve(nextProvision)).status, 200);
  assert.ok(storage.values.has(`invite-use:${PROVISION}`));
  assert.ok(storage.values.has(`invite-use:${nextProvision}`));
  assert.equal(
    [...storage.values.keys()].some((key) =>
      key.startsWith(`invite-use:${generation}:`)
    ),
    false,
  );
  assert.deepEqual(storage.values.get("invite-count"), {
    generation,
    count: 2,
  });
});

test("a committed reservation replays after the invite is deleted", async () => {
  const generation = "2026-07-25T00:00:00.000Z";
  const fingerprint = "a".repeat(64);
  const storage = new Storage();
  const directory = new KV({
    [`invite:${INVITE}`]: {
      enabled: true,
      max_uses: 3,
      uses: 0,
      created_at: generation,
    },
  });
  const authority = new DurableAccountSignup(
    { id: { name: `invite:${INVITE}` }, storage },
    { DIRECTORY: directory },
    { now: () => new Date("2026-07-25T12:00:00.000Z") },
  );
  const reserve = (requestFingerprint = fingerprint) => authority.fetch(
    new Request("https://account-signup.internal/invite/reserve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        invite: INVITE,
        provision_id: PROVISION,
        request_fingerprint: requestFingerprint,
      }),
    }),
  );

  assert.equal((await reserve()).status, 200);
  const useKey = `invite-use:${generation}:${PROVISION}`;
  const committedUse = structuredClone(storage.values.get(useKey));
  await directory.delete(`invite:${INVITE}`);

  const replay = await reserve();
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).uses, 1);
  assert.deepEqual(storage.values.get(useKey), committedUse);
  assert.deepEqual(storage.values.get("invite-count"), {
    generation,
    count: 1,
    use_key_version: 1,
  });
  assert.equal(directory.value(`invite:${INVITE}`), null);
  assert.equal((await reserve("b".repeat(64))).status, 409);
});

test("a recreated code replays the old provision and counts a new one", async () => {
  const firstGeneration = "2026-07-25T00:00:00.000Z";
  const secondGeneration = "2026-08-28T00:00:00.000Z";
  const fingerprint = "a".repeat(64);
  const storage = new Storage();
  const directory = new KV({
    [`invite:${INVITE}`]: {
      enabled: true,
      max_uses: 3,
      uses: 0,
      created_at: firstGeneration,
    },
  });
  const authority = new DurableAccountSignup(
    { id: { name: `invite:${INVITE}` }, storage },
    { DIRECTORY: directory },
    { now: () => new Date("2026-08-28T12:00:00.000Z") },
  );
  const reserve = (provisionID, requestFingerprint = fingerprint) => authority.fetch(
    new Request("https://account-signup.internal/invite/reserve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        invite: INVITE,
        provision_id: provisionID,
        request_fingerprint: requestFingerprint,
      }),
    }),
  );

  assert.equal((await reserve(PROVISION)).status, 200);
  const firstUseKey = `invite-use:${firstGeneration}:${PROVISION}`;
  assert.ok(storage.values.has(firstUseKey));
  const committedUse = structuredClone(storage.values.get(firstUseKey));

  await directory.delete(`invite:${INVITE}`);
  await directory.put(`invite:${INVITE}`, JSON.stringify({
    enabled: true,
    max_uses: 3,
    uses: 0,
    created_at: secondGeneration,
  }));

  const replay = await reserve(PROVISION);
  assert.equal(replay.status, 200);
  assert.equal((await replay.json()).uses, 1);
  assert.ok(storage.values.has(firstUseKey));
  assert.deepEqual(storage.values.get(firstUseKey), committedUse);
  assert.equal(
    storage.values.has(`invite-use:${secondGeneration}:${PROVISION}`),
    false,
  );
  assert.equal(directory.value(`invite:${INVITE}`).uses, 0);
  assert.equal((await reserve(PROVISION, "b".repeat(64))).status, 409);

  const nextProvision = "22222222-2222-4222-8222-222222222222";
  assert.equal((await reserve(nextProvision)).status, 200);
  const secondUseKey = `invite-use:${secondGeneration}:${nextProvision}`;
  assert.ok(storage.values.has(secondUseKey));
  assert.deepEqual(storage.values.get(firstUseKey), committedUse);
  assert.deepEqual(storage.values.get("invite-count"), {
    generation: secondGeneration,
    count: 1,
    use_key_version: 1,
  });
  assert.equal(directory.value(`invite:${INVITE}`).uses, 1);
});

test("invite authority reports its live generation count without mutation", async () => {
  const storage = new Storage();
  const authority = new DurableAccountSignup(
    { id: { name: `invite:${INVITE}` }, storage },
    { DIRECTORY: new KV() },
  );
  const status = () => authority.fetch(
    new Request("https://account-signup.internal/invite/status"),
  );

  const uninitialized = await status();
  assert.equal(uninitialized.status, 200);
  assert.deepEqual(await uninitialized.json(), {
    schema_version: "witself.v0",
    initialized: false,
  });

  await storage.put("invite-count", {
    generation: "2026-08-28T00:00:00.000Z",
    count: 7,
  });
  const initialized = await status();
  assert.equal(initialized.status, 200);
  assert.deepEqual(await initialized.json(), {
    schema_version: "witself.v0",
    initialized: true,
    generation: "2026-08-28T00:00:00.000Z",
    uses: 7,
  });
  assert.deepEqual(storage.values.get("invite-count"), {
    generation: "2026-08-28T00:00:00.000Z",
    count: 7,
  });
});

test("invite verdict exposes independent operator flags", () => {
  const now = Date.parse("2026-08-28T12:00:00.000Z");
  assert.deepEqual(inviteVerdict({
    enabled: false,
    not_before: "2026-08-29T00:00:00.000Z",
    expires_at: "2026-08-28T00:00:00.000Z",
    max_uses: 2,
  }, 2, now), {
    enabled: false,
    exhausted: true,
    expired: true,
    not_yet_valid: true,
    valid: false,
    reason: "disabled",
  });
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

test("closed signup still requires an invite with valid public controls", async () => {
  for (const openValue of [undefined, "false", "TRUE"]) {
    let verifications = 0;
    let counterCalls = 0;
    const env = {
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
      CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
    };
    if (openValue !== undefined) env.CP_SIGNUP_OPEN = openValue;
    const setup = harness({
      env,
      verifyTurnstile: async () => {
        verifications++;
        return { ok: true };
      },
      consumeCounter: async () => {
        counterCalls++;
        return { allowed: true };
      },
    });

    const response = await setup.runtime.fetch(signupRequest({
      invite: "",
      source_ip: "203.0.113.30",
      turnstile_token: "valid-token",
      consent_terms_version: "terms-2026-08-28",
      consent_privacy_version: "privacy-2026-08-28",
    }));
    assert.equal(response.status, 403, String(openValue));
    assert.deepEqual(await response.json(), {
      schema_version: "witself.v0",
      error: "invite code required",
    });
    assert.equal(verifications, 0);
    assert.equal(counterCalls, 0);
    assert.equal(setup.inviteReservations(), 0);
    assert.equal(setup.storage.values.has("account-signup"), false);
  }
});

test("open signup configuration fails closed without affecting invites", async () => {
  for (const scenario of [
    {
      name: "zero limits",
      env: {
        CP_SIGNUP_OPEN: "true",
        CP_SIGNUP_TURNSTILE_ENABLED: "true",
        CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
        CP_SIGNUP_DAILY_LIMIT_PER_IP: "0",
        CP_SIGNUP_DAILY_LIMIT_GLOBAL: "0",
      },
      invitedVerifications: 1,
      invitedCounters: 0,
    },
    {
      name: "Turnstile off",
      env: {
        CP_SIGNUP_OPEN: "true",
        CP_SIGNUP_TURNSTILE_ENABLED: "false",
        CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
        CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
        CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
      },
      invitedVerifications: 0,
      invitedCounters: 2,
    },
  ]) {
    let verifications = 0;
    const counters = [];
    const setup = harness({
      env: scenario.env,
      verifyTurnstile: async () => {
        verifications++;
        return { ok: true };
      },
      consumeCounter: async (input) => {
        counters.push(structuredClone(input));
        return { allowed: true };
      },
    });

    const refused = await setup.runtime.fetch(signupRequest({
      invite: "",
      source_ip: "203.0.113.31",
      turnstile_token: "must-not-be-verified",
    }));
    assert.equal(refused.status, 503, scenario.name);
    assert.deepEqual(await refused.json(), {
      schema_version: "witself.v0",
      error: "open signup configuration is invalid",
    });
    assert.equal(verifications, 0);
    assert.equal(counters.length, 0);
    assert.equal(setup.storage.values.has("account-signup"), false);

    const invited = await setup.runtime.fetch(signupRequest({
      source_ip: "203.0.113.31",
      turnstile_token: "valid-invite-token",
    }));
    assert.equal(invited.status, 201, scenario.name);
    assert.equal(verifications, scenario.invitedVerifications);
    assert.equal(counters.length, scenario.invitedCounters);
    assert.equal(setup.inviteReservations(), 1);
  }
});

test("configured open signup provisions without reserving an invite", async () => {
  const verifications = [];
  const counters = [];
  const setup = harness({
    env: {
      CP_SIGNUP_OPEN: "true",
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
      CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
    },
    verifyTurnstile: async (input) => {
      verifications.push(structuredClone(input));
      return { ok: true };
    },
    consumeCounter: async (input) => {
      counters.push(structuredClone(input));
      return { allowed: true };
    },
  });
  const fields = {
    invite: "",
    source_ip: "203.0.113.32",
    turnstile_token: "one-time-token",
    consent_terms_version: "terms-2026-08-28",
    consent_privacy_version: "privacy-2026-08-28",
  };

  const response = await setup.runtime.fetch(signupRequest(fields));
  assert.equal(response.status, 201);
  assert.deepEqual(verifications, [{
    secretKey: "server-secret",
    token: "one-time-token",
    remoteIp: "203.0.113.32",
  }]);
  assert.equal(counters.length, 2);
  assert.match(counters[0].scope, SIGNUP_IP_SCOPE_PATTERN);
  assert.equal(counters[0].limit, 5);
  assert.equal(counters[1].scope, "signup-counter:global");
  assert.equal(counters[1].limit, 10);
  assert.equal(setup.inviteReservations(), 0);
  assert.equal(setup.placements(), 1);
  assert.deepEqual(
    setup.service.calls.map(({ input }) => ({
      consent_terms_version: input.consent_terms_version,
      consent_privacy_version: input.consent_privacy_version,
    })),
    [{
      consent_terms_version: "terms-2026-08-28",
      consent_privacy_version: "privacy-2026-08-28",
    }],
  );
  assert.equal(
    setup.storage.values.get("account-signup").request_fingerprint,
    "646adc4ef9171944ef38d199d363f783e902ff78827aba441245da929a072461",
  );

  const replay = await setup.runtime.fetch(signupRequest({
    ...fields,
    invite: null,
    source_ip: "198.51.100.32",
    turnstile_token: "replacement-token",
  }));
  assert.equal(replay.status, 201);
  assert.equal(verifications.length, 1);
  assert.equal(counters.length, 2);
  assert.equal(setup.inviteReservations(), 0);
  assert.equal(setup.placements(), 1);
});

test("open signup checks both allowances before requiring consent", async () => {
  for (const { fields, error } of [
    {
      fields: {},
      error:
        "consent_terms_version and consent_privacy_version are required for open signup",
    },
    {
      fields: { consent_terms_version: "terms-2026-08-28" },
      error:
        "consent_terms_version and consent_privacy_version must be provided together",
    },
  ]) {
    const events = [];
    const setup = harness({
      env: {
        CP_SIGNUP_OPEN: "true",
        CP_SIGNUP_TURNSTILE_ENABLED: "true",
        CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
        CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
        CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
      },
      verifyTurnstile: async () => {
        events.push("turnstile");
        return { ok: true };
      },
      consumeCounter: async (input) => {
        events.push(input.scope === "signup-counter:global"
          ? "global"
          : "ip");
        return { allowed: true };
      },
    });

    const response = await setup.runtime.fetch(signupRequest({
      invite: "",
      source_ip: "203.0.113.33",
      turnstile_token: "valid-token",
      ...fields,
    }));
    assert.equal(response.status, 400);
    assert.equal((await response.json()).error, error);
    assert.deepEqual(events, ["turnstile", "ip", "global"]);
    assert.equal(setup.storage.values.has("account-signup"), false);
    assert.equal(setup.inviteReservations(), 0);
  }
});

test("open signup refuses a failed Turnstile before counters", async () => {
  let counterCalls = 0;
  const setup = harness({
    env: {
      CP_SIGNUP_OPEN: "true",
      CP_SIGNUP_TURNSTILE_ENABLED: "true",
      CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
      CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
    },
    verifyTurnstile: async () => ({ ok: false, reason: "invalid" }),
    consumeCounter: async () => {
      counterCalls++;
      return { allowed: true };
    },
  });

  const response = await setup.runtime.fetch(signupRequest({
    invite: "",
    source_ip: "203.0.113.34",
    turnstile_token: "bad-token",
    consent_terms_version: "terms-2026-08-28",
    consent_privacy_version: "privacy-2026-08-28",
  }));
  assert.equal(response.status, 403);
  assert.deepEqual(await response.json(), {
    schema_version: "witself.v0",
    error: "turnstile challenge required",
    challenge_url: "https://self.witwave.ai/signup/challenge",
  });
  assert.equal(counterCalls, 0);
  assert.equal(setup.storage.values.has("account-signup"), false);
  assert.equal(setup.inviteReservations(), 0);
});

test("open signup refuses per-IP and global daily exhaustion", async (t) => {
  const logs = [];
  t.mock.method(console, "log", (...values) => logs.push(values.join(" ")));
  for (const deniedScope of ["ip", "global"]) {
    const calls = [];
    const setup = harness({
      env: {
        CP_SIGNUP_OPEN: "true",
        CP_SIGNUP_TURNSTILE_ENABLED: "true",
        CP_SIGNUP_TURNSTILE_SECRET_KEY: "server-secret",
        CP_SIGNUP_DAILY_LIMIT_PER_IP: "5",
        CP_SIGNUP_DAILY_LIMIT_GLOBAL: "10",
      },
      verifyTurnstile: async () => ({ ok: true }),
      consumeCounter: async (input) => {
        calls.push(structuredClone(input));
        const isGlobal = input.scope === "signup-counter:global";
        return {
          allowed: deniedScope === "global" ? !isGlobal : isGlobal,
        };
      },
    });

    const response = await setup.runtime.fetch(signupRequest({
      invite: "",
      source_ip: "203.0.113.35",
      turnstile_token: "must-not-leak",
      consent_terms_version: "terms-2026-08-28",
      consent_privacy_version: "privacy-2026-08-28",
    }));
    assert.equal(response.status, 429);
    assert.deepEqual(await response.json(), {
      schema_version: "witself.v0",
      error: "signup rate limit exceeded",
    });
    assert.equal(calls.length, deniedScope === "ip" ? 1 : 2);
    assert.match(calls[0].scope, SIGNUP_IP_SCOPE_PATTERN);
    if (deniedScope === "global") {
      assert.equal(calls[1].scope, "signup-counter:global");
    }
    assert.equal(setup.storage.values.has("account-signup"), false);
    assert.equal(setup.inviteReservations(), 0);
  }
  assert.equal(logs.length, 2);
  assert.equal(logs.some((line) => line.includes(PROVISION)), false);
  assert.equal(logs.some((line) => line.includes("203.0.113.35")), false);
  assert.equal(logs.some((line) => line.includes("must-not-leak")), false);
});

test("an invited request is byte-identical when open signup is enabled", async () => {
  const closed = harness({ env: { CP_SIGNUP_OPEN: "false" } });
  const open = harness({ env: { CP_SIGNUP_OPEN: "true" } });

  const closedResponse = await closed.runtime.fetch(signupRequest());
  const openResponse = await open.runtime.fetch(signupRequest());
  assert.equal(openResponse.status, closedResponse.status);
  assert.equal(
    openResponse.headers.get("Content-Type"),
    closedResponse.headers.get("Content-Type"),
  );
  assert.deepEqual(
    new Uint8Array(await openResponse.arrayBuffer()),
    new Uint8Array(await closedResponse.arrayBuffer()),
  );
  assert.deepEqual(
    open.storage.values.get("account-signup"),
    closed.storage.values.get("account-signup"),
  );
  assert.deepEqual(open.service.calls, closed.service.calls);
  assert.deepEqual(open.target.calls, closed.target.calls);
  assert.equal(open.inviteReservations(), closed.inviteReservations());
  assert.equal(open.placements(), closed.placements());
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


function legalManifest(terms = "terms-v2", privacy = "privacy-v2") {
  return Response.json({ terms: { version: terms, path: "/legal/terms" }, privacy: { version: privacy, path: "/legal/privacy" } });
}
function consentRequest(fields = {}) {
  return signupRequest({ consent_terms_version: "terms-v1", consent_privacy_version: "privacy-v1", ...fields });
}
function successorRequest(refusal, candidateID = "successor-one", fields = {}) {
  return { schema_version: "witself.signup-reconsent.v1", refusal, transition_id: "transition-one",
    candidate: { provision_id: candidateID, consent_terms_version: "terms-v2", consent_privacy_version: "privacy-v2" },
    email: "person@example.com", display_name: "Person", invite: INVITE, ...fields };
}
function transitionRequest(input) {
  return new Request("https://account-signup.internal/legal/reconsent", {
    method: "POST", body: JSON.stringify(input), headers: { "Content-Type": "application/json" },
  });
}
function legalCluster({ controls = false } = {}) {
  const nodes = new Map();
  let legalReads = 0, challenges = 0, counterCalls = 0;
  const counterUses = new Map();
  const authority = { response: () => legalManifest() };
  const binding = { idFromName: (name) => name, get: (name) => ({ fetch: (request) => node(name.slice("provision:".length)).runtime.fetch(request) }) };
  const env = { CP_SIGNUP_LEGAL_ENFORCEMENT: "true", ACCOUNT_SIGNUP: binding,
    LEGAL_DOCUMENTS: { fetch: async (request) => {
      legalReads++; assert.equal(request.url, "https://legal.internal/legal/versions.json");
      assert.equal(request.redirect, "error"); return authority.response(request);
    } },
    ...(controls ? { CP_SIGNUP_TURNSTILE_ENABLED: "true", CP_SIGNUP_TURNSTILE_SECRET_KEY: "fixture",
      CP_SIGNUP_DAILY_LIMIT_PER_IP: "1", CP_SIGNUP_DAILY_LIMIT_GLOBAL: "1" } : {}),
  };
  function node(provisionID = PROVISION) {
    if (!nodes.has(provisionID)) nodes.set(provisionID, harness({ provisionID, env,
      verifyTurnstile: async () => { challenges++; return { ok: true }; },
      consumeCounter: async ({ scope, provision_id, limit }) => {
        counterCalls++;
        const old = counterUses.get(scope);
        if (old && old !== provision_id) return { allowed: false };
        counterUses.set(scope, provision_id);
        return { allowed: true, count: 1, limit, day: "2026-09-10", replayed: !!old };
      },
    }));
    return nodes.get(provisionID);
  }
  return { node, nodes, authority, env, counters: () => ({ legalReads, challenges, counterCalls }) };
}
function noProvisioning(setup) {
  assert.equal(setup.inviteReservations(), 0); assert.equal(setup.placements(), 0);
  assert.deepEqual(setup.service.calls, []); assert.deepEqual(setup.target.calls, []);
}

test("legal admission requires exact true and preserves consentless invited signup", async () => {
  for (const enabled of [undefined, "false", "TRUE", "1"]) {
    const setup = harness({ env: { CP_SIGNUP_LEGAL_ENFORCEMENT: enabled, LEGAL_DOCUMENTS: { fetch: () => assert.fail("disabled legal read") } } });
    assert.equal((await setup.runtime.fetch(consentRequest())).status, 201);
    assertNoSignupSecretsOrPII(await setup.storage.get("account-signup"));
  }
  const cluster = legalCluster();
  assert.equal((await cluster.node().runtime.fetch(signupRequest())).status, 201);
  assert.equal(cluster.counters().legalReads, 0);
});

test("fresh legal refusal is durable, private, value-free and terminal before all provisioning", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  const first = await setup.runtime.fetch(consentRequest());
  assert.equal(first.status, 409); assert.equal(first.headers.get("Cache-Control"), "private, no-store");
  const refusal = await first.json();
  assert.equal(refusal.code, "signup_legal_stale"); assert.equal(refusal.required_terms_version, "terms-v2");
  const state = await setup.storage.get("account-signup");
  assert.equal(state.phase, "legal_rejected"); assertNoSignupSecretsOrPII(state); noProvisioning(setup);
  setup.runtime.env.CP_SIGNUP_LEGAL_ENFORCEMENT = "false";
  cluster.authority.response = () => { throw new Error("private upstream value"); };
  assert.deepEqual(await (await setup.runtime.fetch(consentRequest())).json(), refusal);
  assert.equal(cluster.counters().legalReads, 1);
  assert.equal((await setup.runtime.fetch(consentRequest({ email: "changed@example.com" }))).status, 409);
  assert.deepEqual(await setup.storage.get("account-signup"), state); noProvisioning(setup);
});

test("current served consent admits once and admitted recovery needs no legal service", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  const fields = { consent_terms_version: "terms-v2", consent_privacy_version: "privacy-v2" };
  assert.equal((await setup.runtime.fetch(consentRequest(fields))).status, 201);
  const state = await setup.storage.get("account-signup");
  assert.equal(state.phase, "completed"); assert.match(state.legal_admission.manifest_sha256, /^[0-9a-f]{64}$/);
  assertNoSignupSecretsOrPII(state);
  cluster.authority.response = () => { throw new Error("unavailable"); };
  assert.equal((await setup.runtime.fetch(consentRequest(fields))).status, 201);
  assert.equal(cluster.counters().legalReads, 1);
});

test("authority failures preserve a pending checkpoint without claiming durable refusal", async (t) => {
  for (const [name, response] of [
    ["down", () => { throw new Error("private upstream"); }],
    ["status", () => new Response("private upstream", { status: 503 })],
    ["redirect", () => new Response(null, { status: 302, headers: { Location: "https://other.invalid" } })],
    ["duplicate", () => new Response('{"terms":{},"ter\\u006ds":{}}')],
    ["truncated", () => new Response('{"terms":')],
    ["oversized", () => new Response(" ".repeat(65537))],
    ["bad-utf8", () => new Response(new Uint8Array([0xff]))],
    ["wrong-path", () => Response.json({ terms: { version: "v2", path: "/elsewhere" }, privacy: { version: "v2", path: "/legal/privacy" } })],
  ]) await t.test(name, async () => {
    const cluster = legalCluster({ controls: true }); const setup = cluster.node();
    cluster.authority.response = response;
    const first = await setup.runtime.fetch(consentRequest());
    assert.equal(first.status, 503); assert.deepEqual(await first.json(), { schema_version: "witself.v0", error: "signup legal authority is unavailable" });
    const state = await setup.storage.get("account-signup");
    assert.equal(state.phase, "legal_pending"); assert.equal(state.legal_refusal, undefined); assertNoSignupSecretsOrPII(state); noProvisioning(setup);
    cluster.authority.response = () => legalManifest("terms-v1", "privacy-v1");
    assert.equal((await setup.runtime.fetch(consentRequest({ source_ip: "changed" }))).status, 201);
    assert.deepEqual(cluster.counters(), { legalReads: 2, challenges: 1, counterCalls: 2 });
  });
});

test("one trusted successor inherits one completed challenge and both limit-one counter receipts", async () => {
  const cluster = legalCluster({ controls: true }); const original = cluster.node();
  const refusal = await (await original.runtime.fetch(consentRequest())).json();
  const transition = successorRequest(refusal);
  const ack = await original.runtime.fetch(transitionRequest(transition));
  assert.equal(ack.status, 200); assert.equal((await ack.json()).candidate.provision_id, "successor-one");
  assert.deepEqual(cluster.counters(), { legalReads: 1, challenges: 1, counterCalls: 2 });
  const successor = cluster.node("successor-one");
  assert.equal((await successor.runtime.fetch(consentRequest({ ...transition.candidate, source_ip: "new network" }))).status, 201);
  assertNoSignupSecretsOrPII(await original.storage.get("account-signup"));
  assertNoSignupSecretsOrPII(await successor.storage.get("account-signup"));
  assert.deepEqual(cluster.counters(), { legalReads: 2, challenges: 1, counterCalls: 2 });
  cluster.authority.response = () => { throw new Error("down"); };
  assert.equal((await original.runtime.fetch(transitionRequest(transition))).status, 200);
  assert.deepEqual(await (await original.runtime.fetch(consentRequest())).json(), refusal);
});

test("second legal publication between selection and registration reserves the same now-stale candidate", async () => {
  const cluster = legalCluster({ controls: true }); const original = cluster.node();
  const refusal = await (await original.runtime.fetch(consentRequest())).json();
  const transition = successorRequest(refusal);
  cluster.authority.response = () => legalManifest("terms-v3", "privacy-v3");
  assert.equal((await original.runtime.fetch(transitionRequest(transition))).status, 200);
  const successor = cluster.node("successor-one");
  const refusedAgain = await successor.runtime.fetch(consentRequest(transition.candidate));
  assert.equal(refusedAgain.status, 409); const nextRefusal = await refusedAgain.json();
  assert.equal(nextRefusal.required_terms_version, "terms-v3"); noProvisioning(successor);
  const next = successorRequest(nextRefusal, "successor-two", { transition_id: "transition-two",
    candidate: { provision_id: "successor-two", consent_terms_version: "terms-v3", consent_privacy_version: "privacy-v3" } });
  assert.equal((await successor.runtime.fetch(transitionRequest(next))).status, 200);
  assert.equal((await cluster.node("successor-two").runtime.fetch(consentRequest(next.candidate))).status, 201);
  assert.deepEqual(cluster.counters(), { legalReads: 3, challenges: 1, counterCalls: 2 });
});

test("re-consent elects one candidate and rejects changed core, self-target, occupied and opposing targets", async () => {
  const cluster = legalCluster(); const original = cluster.node();
  const refusal = await (await original.runtime.fetch(consentRequest())).json();
  for (const fields of [{ email: "other@example.com" }, { display_name: "Other" }, { invite: "other-invite" }]) {
    assert.equal((await original.runtime.fetch(transitionRequest(successorRequest(refusal, "candidate", fields)))).status, 409);
  }
  assert.equal((await original.runtime.fetch(transitionRequest(successorRequest(refusal, PROVISION)))).status, 400);
  const candidates = await Promise.all(["winner-a", "winner-b"].map((id) => original.runtime.fetch(transitionRequest(successorRequest(refusal, id)))));
  assert.deepEqual(candidates.map((v) => v.status).sort(), [200, 409]);
  assert.equal([...cluster.nodes.values()].filter((v) => v.storage.values.get("account-signup")?.phase === "legal_reserved").length, 1);

  const opposing = legalCluster();
  const a = opposing.node("attempt-a"), b = opposing.node("attempt-b");
  const ar = await (await a.runtime.fetch(consentRequest({ provision_id: "attempt-a" }))).json();
  const br = await (await b.runtime.fetch(consentRequest({ provision_id: "attempt-b" }))).json();
  const outcomes = await Promise.all([
    a.runtime.fetch(transitionRequest(successorRequest(ar, "attempt-b"))),
    b.runtime.fetch(transitionRequest(successorRequest(br, "attempt-a"))),
  ]);
  assert.deepEqual(outcomes.map((v) => v.status), [409, 409]); noProvisioning(a); noProvisioning(b);
});

test("refusal, preparation, reservation and registration replay through before/after durable write crashes", async (t) => {
  for (const boundary of ["legal_pending", "legal_rejected", "prepared", "legal_reserved", "registered"]) {
    for (const after of [false, true]) await t.test(`${boundary}: ${after ? "after" : "before"}`, async () => {
      const cluster = legalCluster({ controls: true }); const original = cluster.node(); const candidate = cluster.node("successor-one");
      const target = boundary === "legal_reserved" ? candidate : original;
      const put = target.storage.put.bind(target.storage); let armed = true;
      target.storage.put = async (key, state) => {
        const hit = state.phase === boundary || (boundary === "prepared" && state.legal_successor?.registered === false) ||
          (boundary === "registered" && state.legal_successor?.registered === true);
        if (hit && armed) {
          armed = false; if (after) await put(key, state);
          throw new Error("simulated durable write failure");
        }
        return put(key, state);
      };
      let initial = await original.runtime.fetch(consentRequest());
      if (["legal_pending", "legal_rejected"].includes(boundary)) {
        assert.equal(initial.status, 500); initial = await original.runtime.fetch(consentRequest());
      }
      assert.equal(initial.status, 409); const refusal = await initial.json();
      const transition = successorRequest(refusal);
      let ack = await original.runtime.fetch(transitionRequest(transition));
      if (!["legal_pending", "legal_rejected"].includes(boundary)) {
        assert.ok(ack.status >= 500); ack = await original.runtime.fetch(transitionRequest(transition));
      }
      assert.equal(ack.status, 200);
      assert.equal((await candidate.runtime.fetch(consentRequest(transition.candidate))).status, 201);
      assert.equal(cluster.counters().challenges, 1);
      assert.equal(cluster.counters().counterCalls, boundary === "legal_pending" && !after ? 4 : 2);
      assert.equal(original.service.calls.length, 0); assert.equal(candidate.service.receipts.size, 1);
    });
  }
});

test("reserved lineage never trusts public abuse flags or bypasses changed policy", async () => {
  const cluster = legalCluster({ controls: true }); const original = cluster.node();
  const refusal = await (await original.runtime.fetch(consentRequest())).json();
  const transition = successorRequest(refusal);
  assert.equal((await original.runtime.fetch(transitionRequest({ ...transition, turnstile_verified: true }))).status, 400);
  assert.equal((await original.runtime.fetch(transitionRequest(transition))).status, 200);
  const candidate = cluster.node("successor-one");
  candidate.runtime.env.CP_SIGNUP_DAILY_LIMIT_GLOBAL = "2";
  candidate.runtime.env.CP_SIGNUP_LEGAL_ENFORCEMENT = "false";
  assert.equal((await candidate.runtime.fetch(consentRequest({ ...transition.candidate, turnstile_verified: true, legal_abuse: {} }))).status, 503);
  noProvisioning(candidate);
  const state = await candidate.storage.get("account-signup");
  state.legal_abuse.global = null;
  candidate.storage.values.set("account-signup", state);
  assert.equal((await candidate.runtime.fetch(consentRequest(transition.candidate))).status, 500);
});

test("every admitted historical checkpoint bypasses new legal enforcement after restart", async (t) => {
  for (const phase of ["initialized", "invite_reserved", "cell_selected", "protocol_verified", "target_reserved",
    "cell_acknowledged", "target_attached", "pending_projected", "route_projected", "resident_promoted", "completed"]) {
    await t.test(phase, async () => {
      const setup = harness(); const put = setup.storage.put.bind(setup.storage); let armed = true;
      setup.storage.put = async (key, state) => {
        await put(key, state);
        if (armed && state.phase === phase) { armed = false; throw new Error("crash after historical admission"); }
      };
      assert.equal((await setup.runtime.fetch(consentRequest())).status, 500);
      assert.equal((await setup.storage.get("account-signup")).phase, phase);
      const resumed = harness({ storage: setup.storage, directory: setup.directory, service: setup.service, target: setup.target,
        env: { CP_SIGNUP_LEGAL_ENFORCEMENT: "true", LEGAL_DOCUMENTS: { fetch: () => assert.fail("admitted legal lookup") } } });
      assert.equal((await resumed.runtime.fetch(consentRequest())).status, 201);
      assert.equal(setup.service.receipts.size, 1);
    });
  }
});

test("initialized write is the legal admission boundary before or after a crash", async (t) => {
  for (const after of [false, true]) await t.test(String(after), async () => {
    const cluster = legalCluster({ controls: true }); const setup = cluster.node();
    cluster.authority.response = () => legalManifest("terms-v1", "privacy-v1");
    const put = setup.storage.put.bind(setup.storage); let armed = true;
    setup.storage.put = async (key, state) => {
      if (armed && state.phase === "initialized") {
        armed = false; if (after) await put(key, state); throw new Error("admission write interrupted");
      }
      return put(key, state);
    };
    assert.equal((await setup.runtime.fetch(consentRequest())).status, 500); noProvisioning(setup);
    cluster.authority.response = () => legalManifest("terms-v2", "privacy-v2");
    assert.equal((await setup.runtime.fetch(consentRequest())).status, after ? 201 : 409);
    assert.deepEqual(cluster.counters(), { legalReads: after ? 1 : 2, challenges: 1, counterCalls: 2 });
  });
});

test("canceled canonical read cannot admit later when the service eventually responds", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  let resolve, reading;
  const started = new Promise((r) => { reading = r; });
  cluster.authority.response = () => new Promise((r) => { resolve = r; reading(); });
  const controller = new AbortController();
  const pending = setup.runtime.fetch(new Request(consentRequest(), { signal: controller.signal }));
  await started; controller.abort();
  const response = await pending;
  assert.equal(response.status, 503);
  const before = await setup.storage.get("account-signup"); assert.equal(before.phase, "legal_pending");
  resolve(legalManifest("terms-v1", "privacy-v1"));
  await Promise.resolve(); await Promise.resolve();
  assert.deepEqual(await setup.storage.get("account-signup"), before); noProvisioning(setup);
});

test("non-string manifest labels never persist an unusable terminal refusal", async (t) => {
  for (const version of [123, true, {}, ["v2"], null]) await t.test(JSON.stringify(version), async () => {
    const cluster = legalCluster(); const setup = cluster.node();
    cluster.authority.response = () => Response.json({ terms: { version, path: "/legal/terms" }, privacy: { version: "v2", path: "/legal/privacy" } });
    assert.equal((await setup.runtime.fetch(consentRequest())).status, 503);
    const state = await setup.storage.get("account-signup");
    assert.equal(state.phase, "legal_pending"); assert.equal(state.legal_refusal, undefined); noProvisioning(setup);
    cluster.authority.response = () => legalManifest("terms-v1", "privacy-v1");
    assert.equal((await setup.runtime.fetch(consentRequest())).status, 201);
  });
});

test("new request-size bound does not retroactively reject an admitted large historical core", async () => {
  const setup = harness(); const email = "a".repeat(33000) + "@example.com";
  const put = setup.storage.put.bind(setup.storage); let armed = true;
  setup.storage.put = async (key, state) => {
    await put(key, state);
    if (armed && state.phase === "initialized") { armed = false; throw new Error("crash after admission"); }
  };
  assert.equal((await setup.runtime.fetch(consentRequest({ email }))).status, 500);
  const resumed = harness({ storage: setup.storage, env: { CP_SIGNUP_LEGAL_ENFORCEMENT: "true",
    LEGAL_DOCUMENTS: { fetch: () => assert.fail("historical legal lookup") } } });
  assert.equal((await resumed.runtime.fetch(consentRequest({ email }))).status, 201);
});

test("an unproven newly enabled challenge preserves the historical preflight for recovery", async () => {
  const storage = new Storage(); let first = true;
  const setup = harness({ storage, env: { CP_SIGNUP_DAILY_LIMIT_PER_IP: "1", CP_SIGNUP_DAILY_LIMIT_GLOBAL: "1" },
    consumeCounter: async () => { if (first) { first = false; throw new Error("ambiguous old counter"); }
      return { allowed: true, count: 1, limit: 1, day: "2026-09-10" }; } });
  assert.equal((await setup.runtime.fetch(consentRequest())).status, 500);
  const before = await storage.get("account-signup"); assert.equal(before.phase, "abuse_preflight");
  Object.assign(setup.runtime.env, { CP_SIGNUP_LEGAL_ENFORCEMENT: "true", CP_SIGNUP_TURNSTILE_ENABLED: "true",
    CP_SIGNUP_TURNSTILE_SECRET_KEY: "fixture", LEGAL_DOCUMENTS: { fetch: () => assert.fail("unproven policy read") } });
  assert.equal((await setup.runtime.fetch(consentRequest())).status, 503);
  assert.deepEqual(await storage.get("account-signup"), before); noProvisioning(setup);
});

test("a canceled reservation-header wait preserves its elected candidate and cannot finalize late", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  const refusal = await (await setup.runtime.fetch(consentRequest())).json(); const transition = successorRequest(refusal);
  const binding = setup.runtime.env.ACCOUNT_SIGNUP; let release, started;
  const waiting = new Promise((r) => { started = r; });
  setup.runtime.env.ACCOUNT_SIGNUP = { idFromName: (name) => name, get: () => ({ fetch: () => new Promise((r) => { release = r; started(); }) }) };
  const controller = new AbortController();
  const response = setup.runtime.fetch(new Request(transitionRequest(transition), { signal: controller.signal }));
  await waiting; controller.abort();
  assert.ok((await response).status >= 500);
  const before = await setup.storage.get("account-signup"); assert.equal(before.legal_successor.registered, false);
  release(Response.json({ status: "registered" })); await Promise.resolve(); await Promise.resolve();
  assert.deepEqual(await setup.storage.get("account-signup"), before); noProvisioning(setup);
  setup.runtime.env.ACCOUNT_SIGNUP = binding;
  assert.equal((await setup.runtime.fetch(transitionRequest(transition))).status, 200);
});


test("a maximum invariant core accepts every bounded successor ID and legal pair", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  const core = { email: "a@example.com", display_name: "Person", invite: INVITE };
  core.email = "a".repeat(32768 - new TextEncoder().encode(JSON.stringify(core)).length) + core.email;
  assert.equal(new TextEncoder().encode(JSON.stringify(core)).length, 32768);
  const response = await setup.runtime.fetch(consentRequest(core));
  assert.equal(response.status, 409); const refusal = await response.json();
  const candidate = { provision_id: "c".repeat(128), consent_terms_version: "t".repeat(64), consent_privacy_version: "p".repeat(64) };
  const request = successorRequest(refusal, candidate.provision_id, { email: core.email, candidate });
  assert.equal((await setup.runtime.fetch(transitionRequest(request))).status, 200);
  noProvisioning(setup);
  cluster.authority.response = () => legalManifest(candidate.consent_terms_version, candidate.consent_privacy_version);
  const successor = cluster.node(candidate.provision_id);
  assert.equal((await successor.runtime.fetch(consentRequest({ ...core, ...candidate }))).status, 201);
  assert.equal(successor.service.receipts.size, 1);
});

test("oversized invariant core and lone surrogates fail before fresh admission or candidate election", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  for (const email of ["a".repeat(33000) + "@example.com", "<&>".repeat(1900) + "@example.com", "\ud800@example.com"]) {
    assert.equal((await setup.runtime.fetch(consentRequest({ email }))).status, 400);
    assert.equal(await setup.storage.get("account-signup"), undefined);
  }
  assert.deepEqual(cluster.counters(), { legalReads: 0, challenges: 0, counterCalls: 0 }); noProvisioning(setup);
  const refusal = await (await setup.runtime.fetch(consentRequest())).json();
  const before = await setup.storage.get("account-signup");
  assert.equal((await setup.runtime.fetch(transitionRequest(successorRequest(refusal, "candidate", { email: "a".repeat(33000) + "@example.com" })))).status, 400);
  assert.deepEqual(await setup.storage.get("account-signup"), before);
  assert.equal(cluster.nodes.has("candidate"), false); noProvisioning(setup);
});

test("unchanged Go-normalized FEFF input reconsents through the original CP-normalized core", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  const fields = { email: "\ufeffperson@example.com", display_name: "\ufeffPerson\ufeff" };
  const refusal = await (await setup.runtime.fetch(consentRequest(fields))).json();
  assert.equal(refusal.code, "signup_legal_stale");
  const transition = successorRequest(refusal, "feff-successor", fields);
  assert.equal((await setup.runtime.fetch(transitionRequest(transition))).status, 200);
  const successor = cluster.node("feff-successor");
  assert.equal((await successor.runtime.fetch(consentRequest({ ...fields, ...transition.candidate }))).status, 201);
  assert.equal(successor.service.receipts.get("feff-successor").request.email, "person@example.com");
  noProvisioning(setup);
});

test("oversized incoming core cannot hide behind normalization before admission or election", async () => {
  const cluster = legalCluster(); const setup = cluster.node();
  const email = "\ufeff".repeat(11000) + "person@example.com";
  assert.equal(email.trim(), "person@example.com");
  assert.equal((await setup.runtime.fetch(consentRequest({ email }))).status, 400);
  assert.equal(await setup.storage.get("account-signup"), undefined); noProvisioning(setup);
  const refusal = await (await setup.runtime.fetch(consentRequest())).json();
  const before = await setup.storage.get("account-signup");
  assert.equal((await setup.runtime.fetch(transitionRequest(successorRequest(refusal, "oversized-wire", { email })))).status, 400);
  assert.deepEqual(await setup.storage.get("account-signup"), before);
  assert.equal(cluster.nodes.has("oversized-wire"), false);
  // A terminal exact canonical replay keeps the original refusal even when
  // another wire representation is larger; it creates no new attempt.
  assert.deepEqual(await (await setup.runtime.fetch(consentRequest({ email }))).json(), refusal);
});

test("fresh legal wire bounds retain omitted and null display defaults and open invite omission", async () => {
  for (const display_name of [undefined, null, ""]) {
    const cluster = legalCluster(); const setup = cluster.node();
    assert.equal((await setup.runtime.fetch(consentRequest({ display_name }))).status, 409);
    noProvisioning(setup);
  }
  const cluster = legalCluster({ controls: true }); const setup = cluster.node();
  setup.runtime.env.CP_SIGNUP_OPEN = "true";
  assert.equal((await setup.runtime.fetch(consentRequest({ invite: undefined }))).status, 409);
  noProvisioning(setup);
});
