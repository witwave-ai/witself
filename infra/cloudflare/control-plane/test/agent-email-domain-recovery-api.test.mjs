import assert from "node:assert/strict";
import test from "node:test";

import {
  AGENT_EMAIL_DOMAIN_RECOVERIES_PATH,
  AGENT_EMAIL_DOMAIN_RECOVERY_HEADER,
  handleAgentEmailDomainRecoveryAdminRequest,
  isAgentEmailDomainRecoveryAdminPath,
} from "../src/agent-email-domain-recovery-api.mjs";

const ADMIN = { admin_id: "adm_recovery" };
const SECRET = "d".repeat(48);

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
      AGENT_EMAIL_DOMAINS: namespace,
      CP_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN: SECRET,
    },
  };
}

function request(path, method = "GET", body, secret = SECRET) {
  return new Request(`https://self.example${path}`, {
    method,
    headers: {
      ...(secret === null ? {} : { [AGENT_EMAIL_DOMAIN_RECOVERY_HEADER]: secret }),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

test("custom-domain recovery admin matcher is exact", () => {
  for (const path of [
    "/v1/admin/agent-email-domain-journal",
    "/v1/admin/agent-email-domain-journal:bootstrap",
    "/v1/admin/agent-email-domain-journal:checkpoint",
    "/v1/admin/agent-email-domain-recoveries",
    "/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa",
    "/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:advance",
    "/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:verify",
  ]) assert.equal(isAgentEmailDomainRecoveryAdminPath(path), true, path);
  assert.equal(isAgentEmailDomainRecoveryAdminPath(
    "/v1/admin/agent-email-domain-recoveries/aedrec_bad:advance",
  ), false);
  assert.equal(isAgentEmailDomainRecoveryAdminPath(
    "/v1/admin/agent-email-domain-recoveries/extra",
  ), false);
});

test("custom-domain recovery requires both admin and distinct credential", async () => {
  const { env, calls } = harness();
  const url = new URL(`https://self.example${AGENT_EMAIL_DOMAIN_RECOVERIES_PATH}`);
  const noAdmin = await handleAgentEmailDomainRecoveryAdminRequest(
    request(url.pathname, "POST", {}), env, url, null,
  );
  assert.equal(noAdmin.status, 401);
  const noRecoveryCredential = await handleAgentEmailDomainRecoveryAdminRequest(
    request(url.pathname, "POST", {}, null), env, url, ADMIN,
  );
  assert.equal(noRecoveryCredential.status, 401);
  const wrongRecoveryCredential = await handleAgentEmailDomainRecoveryAdminRequest(
    request(url.pathname, "POST", {}, "wrong"), env, url, ADMIN,
  );
  assert.equal(wrongRecoveryCredential.status, 401);
  assert.equal(calls.length, 0);
});

test("empty-target recovery dispatches only fences to a named object", async () => {
  const { env, calls } = harness();
  const url = new URL(`https://self.example${AGENT_EMAIL_DOMAIN_RECOVERIES_PATH}`);
  const response = await handleAgentEmailDomainRecoveryAdminRequest(
    request(url.pathname, "POST", {
      recovery_id: "aedrec_aaaaaaaaaaaaaaaa",
      source_stream_id: "aedj_bbbbbbbbbbbbbbbb",
      expected_head: { sequence: 42, hash: "c".repeat(64) },
      reason: "quarterly empty-target restore drill",
      idempotency_key: "restore-drill-2026-08-08",
    }),
    env,
    url,
    ADMIN,
  );
  assert.equal(response.status, 200);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].object, "recovery:aedrec_aaaaaaaaaaaaaaaa");
  assert.equal(new URL(calls[0].url).pathname, "/recovery/start");
  assert.deepEqual(calls[0].body.expected_head, {
    sequence: 42,
    hash: "c".repeat(64),
  });
  assert.equal(calls[0].body.active_object_name, "global");
  assert.equal(calls[0].body.target_object_name,
    "recovery:aedrec_aaaaaaaaaaaaaaaa");
  assert.equal(
    calls[0].init.headers[AGENT_EMAIL_DOMAIN_RECOVERY_HEADER],
    undefined,
  );
});

test("journal maintenance always targets fixed global authority", async () => {
  const { env, calls } = harness();
  env.CP_AGENT_EMAIL_DOMAIN_REGISTRY_OBJECT = "authority-primary";
  for (const action of ["bootstrap", "checkpoint"]) {
    const path = `/v1/admin/agent-email-domain-journal:${action}`;
    const url = new URL(`https://self.example${path}`);
    const response = await handleAgentEmailDomainRecoveryAdminRequest(
      request(path, "POST", {
        reason: `${action} portable custom-domain authority`,
        idempotency_key: `journal-${action}-v1`,
      }),
      env,
      url,
      ADMIN,
    );
    assert.equal(response.status, 200);
  }
  assert.deepEqual(
    calls.map(({ object, url, body }) => ({
      object,
      path: new URL(url).pathname,
      actor: body.actor,
    })),
    [
      {
        object: "global",
        path: "/journal/bootstrap",
        actor: { kind: "platform_admin", id: "adm_recovery" },
      },
      {
        object: "global",
        path: "/journal/checkpoint",
        actor: { kind: "platform_admin", id: "adm_recovery" },
      },
    ],
  );
});

test("recovery actions forward the exact current action fence", async () => {
  const { env, calls } = harness();
  const recoveryID = "aedrec_aaaaaaaaaaaaaaaa";
  const expectedActionFence = "d".repeat(64);

  for (const action of ["advance", "verify"]) {
    const path =
      `/v1/admin/agent-email-domain-recoveries/${recoveryID}:${action}`;
    const url = new URL(`https://self.example${path}`);
    const response = await handleAgentEmailDomainRecoveryAdminRequest(
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

test("recovery actions reject noncanonical action fences before dispatch", async () => {
  const { env, calls } = harness();
  const recoveryID = "aedrec_aaaaaaaaaaaaaaaa";
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
      `/v1/admin/agent-email-domain-recoveries/${recoveryID}:${action}`;
    const url = new URL(`https://self.example${path}`);
    for (const body of invalidBodies) {
      const response = await handleAgentEmailDomainRecoveryAdminRequest(
        request(path, "POST", body), env, url, ADMIN,
      );
      assert.equal(response.status, 400);
      assert.deepEqual(await response.json(), {
        schema_version: "witself.agent-email-domain-recovery.v1",
        error: "idempotency_key and expected_action_fence are required",
      });
    }
  }
  assert.equal(calls.length, 0);
});
