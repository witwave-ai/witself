import assert from "node:assert/strict";
import test from "node:test";

import { DurableAccountSignup } from "../src/account-signup-runtime.mjs";
import {
  DurableTargetCellCoordinator,
} from "../src/target-cell-coordinator.mjs";

const PROVISION = "11111111-1111-4111-8111-111111111111";
const ACCOUNT = "acct_signup";
const INVITE = "early-access";

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
    return Response.json({
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
    }, { status: 201 });
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
} = {}) {
  directory ??= new KV(Object.fromEntries(
    cells.map((entry) => [`cell:${entry.name}`, entry]),
  ));
  let placements = 0;
  let inviteReservations = 0;
  const runtime = new DurableAccountSignup(
    { id: { name: `provision:${PROVISION}` }, storage },
    { DIRECTORY: directory },
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
