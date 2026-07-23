import assert from "node:assert/strict";
import test from "node:test";

import {
  activatePlanLifecycle,
  activationConfig,
} from "../scripts/activate-plan-lifecycle.mjs";

test("activation operator config reads the bridge credential only from env", () => {
  assert.deepEqual(activationConfig({
    INTERNAL_BRIDGE_TOKEN: " bridge-secret ",
    WITSELF_CONTROL_PLANE: "https://control.example/",
  }), {
    endpoint:
      "https://control.example/v1/internal/plan-lifecycle:activate",
    token: "bridge-secret",
  });
  assert.throws(
    () => activationConfig({}),
    /INTERNAL_BRIDGE_TOKEN must be set in the environment/,
  );
  assert.throws(
    () => activationConfig({
      INTERNAL_BRIDGE_TOKEN: "bridge-secret",
      WITSELF_CONTROL_PLANE: "http://control.example",
    }),
    /valid HTTPS URL/,
  );
});

test("activation operator sends the token as authorization and verifies proof", async () => {
  let call;
  await activatePlanLifecycle({
    endpoint: "https://control.example/v1/internal/plan-lifecycle:activate",
    token: "bridge-secret",
  }, async (url, init) => {
    call = { url, init };
    return new Response(JSON.stringify({
      schema_version: "witself.v0",
      plan_lifecycle: {
        activated: true,
        enabled: true,
      },
    }), {
      headers: { "Content-Type": "application/json" },
    });
  });

  assert.equal(
    call.url,
    "https://control.example/v1/internal/plan-lifecycle:activate",
  );
  assert.equal(call.init.method, "POST");
  assert.equal(call.init.headers.Authorization, "Bearer bridge-secret");
  assert.equal(call.init.body, undefined);
  assert.equal(call.init.signal instanceof AbortSignal, true);
});

test("activation operator reports sanitized failures", async () => {
  for (const fetchImpl of [
    async () => {
      throw new Error("bridge-secret transport detail");
    },
    async () => new Response("bridge-secret response detail", { status: 503 }),
    async () => new Response(JSON.stringify({
      schema_version: "witself.v0",
      plan_lifecycle: {
        activated: true,
        enabled: false,
        detail: "bridge-secret",
      },
    })),
  ]) {
    await assert.rejects(
      activatePlanLifecycle({
        endpoint:
          "https://control.example/v1/internal/plan-lifecycle:activate",
        token: "bridge-secret",
      }, fetchImpl),
      (error) => {
        assert.equal(error.message.includes("bridge-secret"), false);
        return true;
      },
    );
  }
});
