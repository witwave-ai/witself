import assert from "node:assert/strict";
import test from "node:test";

import {
  handleRealmEmailAliasRecoveryAdminRequest,
  isRealmEmailAliasRecoveryAdminPath,
  REALM_EMAIL_ALIAS_RECOVERIES_PATH,
  REALM_EMAIL_ALIAS_RECOVERY_HEADER,
} from "../src/realm-email-alias-recovery-api.mjs";

const ADMIN = { admin_id: "adm_recovery" };
const SECRET = "r".repeat(48);

function harness(response = Response.json({ ok: true })) {
  const calls = [];
  const namespace = {
    idFromName: (name) => ({ name }),
    get: (id) => ({
      fetch: async (url, init) => {
        calls.push({ object: id.name, url, init, body: JSON.parse(init.body) });
        return response.clone();
      },
    }),
  };
  return {
    calls,
    env: {
      REALM_EMAIL_ALIASES: namespace,
      CP_REALM_EMAIL_ALIAS_RECOVERY_TOKEN: SECRET,
    },
  };
}

function request(path, method = "GET", body, secret = SECRET) {
  return new Request(`https://self.example${path}`, {
    method,
    headers: {
      ...(secret === null ? {} : { [REALM_EMAIL_ALIAS_RECOVERY_HEADER]: secret }),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

test("recovery admin matcher is exact", () => {
  for (const path of [
    "/v1/admin/realm-email-alias-journal",
    "/v1/admin/realm-email-alias-journal:bootstrap",
    "/v1/admin/realm-email-alias-journal:checkpoint",
    "/v1/admin/realm-email-alias-recoveries",
    "/v1/admin/realm-email-alias-recoveries/rear_aaaaaaaaaaaaaaaa",
    "/v1/admin/realm-email-alias-recoveries/rear_aaaaaaaaaaaaaaaa:advance",
    "/v1/admin/realm-email-alias-recoveries/rear_aaaaaaaaaaaaaaaa:verify",
  ]) assert.equal(isRealmEmailAliasRecoveryAdminPath(path), true, path);
  assert.equal(isRealmEmailAliasRecoveryAdminPath(
    "/v1/admin/realm-email-alias-recoveries/rear_bad:advance",
  ), false);
  assert.equal(isRealmEmailAliasRecoveryAdminPath(
    "/v1/admin/realm-email-alias-recoveries/extra",
  ), false);
});

test("recovery administration requires both authenticated admin and distinct credential", async () => {
  const { env, calls } = harness();
  const url = new URL(`https://self.example${REALM_EMAIL_ALIAS_RECOVERIES_PATH}`);
  const noAdmin = await handleRealmEmailAliasRecoveryAdminRequest(
    request(url.pathname, "POST", {}), env, url, null,
  );
  assert.equal(noAdmin.status, 401);
  const noRecoveryCredential = await handleRealmEmailAliasRecoveryAdminRequest(
    request(url.pathname, "POST", {}, null), env, url, ADMIN,
  );
  assert.equal(noRecoveryCredential.status, 401);
  const wrongRecoveryCredential = await handleRealmEmailAliasRecoveryAdminRequest(
    request(url.pathname, "POST", {}, "wrong"), env, url, ADMIN,
  );
  assert.equal(wrongRecoveryCredential.status, 401);
  assert.equal(calls.length, 0);
});

test("empty-target recovery dispatches only value-free fences to a distinct named object", async () => {
  const { env, calls } = harness();
  const url = new URL(`https://self.example${REALM_EMAIL_ALIAS_RECOVERIES_PATH}`);
  const response = await handleRealmEmailAliasRecoveryAdminRequest(
    request(url.pathname, "POST", {
      recovery_id: "rear_aaaaaaaaaaaaaaaa",
      source_stream_id: "reaj_bbbbbbbbbbbbbbbb",
      expected_head: { sequence: 42, hash: "c".repeat(64) },
      reason: "quarterly empty-target restore drill",
      idempotency_key: "restore-drill-2026-08-02",
    }),
    env,
    url,
    ADMIN,
  );
  assert.equal(response.status, 200);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].object, "recovery:rear_aaaaaaaaaaaaaaaa");
  assert.equal(new URL(calls[0].url).pathname, "/recovery/start");
  assert.deepEqual(calls[0].body.expected_head, {
    sequence: 42,
    hash: "c".repeat(64),
  });
  assert.equal(calls[0].body.active_object_name, "global");
  assert.equal(calls[0].body.target_object_name,
    "recovery:rear_aaaaaaaaaaaaaaaa");
  assert.equal(calls[0].init.headers[REALM_EMAIL_ALIAS_RECOVERY_HEADER], undefined);
});

test("journal bootstrap always targets fixed global authority", async () => {
  const { env, calls } = harness();
  // A stale or misspelled deployment variable is deliberately ignored. There
  // is no supported cutover selector in this release.
  env.CP_REALM_EMAIL_ALIAS_REGISTRY_OBJECT = "authority-primary";
  const path = "/v1/admin/realm-email-alias-journal:bootstrap";
  const url = new URL(`https://self.example${path}`);
  const response = await handleRealmEmailAliasRecoveryAdminRequest(
    request(path, "POST", {
      reason: "establish portable authority baseline",
      idempotency_key: "journal-bootstrap-v1",
    }),
    env,
    url,
    ADMIN,
  );
  assert.equal(response.status, 200);
  assert.equal(calls[0].object, "global");
  assert.equal(new URL(calls[0].url).pathname, "/journal/bootstrap");
  assert.deepEqual(calls[0].body.actor, {
    kind: "platform_admin",
    id: "adm_recovery",
  });
});

test("recovery actions forward the exact current action fence", async () => {
  const { env, calls } = harness();
  const recoveryID = "rear_aaaaaaaaaaaaaaaa";
  const expectedActionFence = "d".repeat(64);

  for (const action of ["advance", "verify"]) {
    const path =
      `/v1/admin/realm-email-alias-recoveries/${recoveryID}:${action}`;
    const url = new URL(`https://self.example${path}`);
    const response = await handleRealmEmailAliasRecoveryAdminRequest(
      request(path, "POST", {
        idempotency_key: `restore-${action}-1`,
        expected_action_fence: expectedActionFence,
      }),
      env,
      url,
      ADMIN,
    );
    assert.equal(response.status, 200);
  }

  assert.equal(calls.length, 2);
  assert.deepEqual(
    calls.map(({ object, url, body }) => ({
      object,
      path: new URL(url).pathname,
      body,
    })),
    [
      {
        object: `recovery:${recoveryID}`,
        path: "/recovery/advance",
        body: {
          actor: { kind: "platform_admin", id: "adm_recovery" },
          recovery_id: recoveryID,
          idempotency_key: "restore-advance-1",
          expected_action_fence: expectedActionFence,
        },
      },
      {
        object: `recovery:${recoveryID}`,
        path: "/recovery/verify",
        body: {
          actor: { kind: "platform_admin", id: "adm_recovery" },
          recovery_id: recoveryID,
          idempotency_key: "restore-verify-1",
          expected_action_fence: expectedActionFence,
        },
      },
    ],
  );
});

test("recovery actions reject missing or noncanonical action fences before dispatch", async () => {
  const { env, calls } = harness();
  const recoveryID = "rear_aaaaaaaaaaaaaaaa";
  const invalidBodies = [
    { idempotency_key: "restore-step-missing" },
    {
      idempotency_key: "restore-step-short",
      expected_action_fence: "a".repeat(63),
    },
    {
      idempotency_key: "restore-step-uppercase",
      expected_action_fence: "A".repeat(64),
    },
  ];

  for (const action of ["advance", "verify"]) {
    const path =
      `/v1/admin/realm-email-alias-recoveries/${recoveryID}:${action}`;
    const url = new URL(`https://self.example${path}`);
    for (const body of invalidBodies) {
      const response = await handleRealmEmailAliasRecoveryAdminRequest(
        request(path, "POST", body),
        env,
        url,
        ADMIN,
      );
      assert.equal(response.status, 400);
      assert.deepEqual(await response.json(), {
        schema_version: "witself.realm-email-alias-recovery.v1",
        error: "idempotency_key and expected_action_fence are required",
      });
    }
  }

  assert.equal(calls.length, 0);
});
