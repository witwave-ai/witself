import assert from "node:assert/strict";
import test from "node:test";

import {
  applyRollbackPlan,
  canonicalJSON,
  createRollbackPlan,
  parseArgs,
  rollbackDeploymentArguments,
  rollbackLiveRuntime,
  rollbackOperationsLeaseRuntime,
  sha256,
  verifyPlan,
} from "../scripts/rollback.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
  WRANGLER_PRODUCTION_ENV_FILE,
} from "../scripts/wrangler-environment.mjs";

const deploymentID = "11111111-2222-4333-8444-555555555555";
const currentID = "66666666-7777-4888-8999-aaaaaaaaaaaa";
const candidateID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee";
const postDeploymentID = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff";
const directoryID = "b".repeat(32);
const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
const keyring = JSON.stringify({ "route-2026-08": publicKey });

function plain(name, text) {
  return { name, text, type: "plain_text" };
}

function version({
  id,
  number,
  releaseVersion,
  commit,
  date,
} = {}) {
  return {
    id,
    number,
    resources: {
      script: {
        etag: `provider-artifact-etag-${number}-1234567890`,
        handlers: ["email"],
      },
      script_runtime: {
        compatibility_date: "2026-07-21",
        compatibility_flags: ["global_fetch_strictly_public"],
      },
      bindings: [
        plain("AGENT_EMAIL_DOMAIN", "witmail.net"),
        plain("AGENT_EMAIL_LEGACY_DOMAINS", "agent-mail.witwave.ai"),
        plain("AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST", ""),
        plain("AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS", keyring),
        { name: "CONTROL_PLANE_EDGE_TOKEN", type: "secret_text" },
        plain("CONTROL_PLANE_URL", "https://self.witwave.ai/"),
        { name: "EMAIL_DIRECTORY", namespace_id: directoryID, type: "kv_namespace" },
        { name: "EMAIL_EDGE_METRICS", dataset: "witself_agent_email_edge", type: "analytics_engine" },
        plain("REALM_EMAIL_ALIAS_DELIVERY_ENABLED", "false"),
        plain("REALM_EMAIL_CANONICAL_DELIVERY_ENABLED", "false"),
        {
          name: "REALM_ROUTE_COLD_MISS_LIMITER",
          namespace_id: "2201",
          simple: { limit: 10, period: 10 },
          type: "ratelimit",
        },
        {
          name: "REALM_ROUTE_KNOWN_MISS_LIMITER",
          namespace_id: "2202",
          simple: { limit: 100, period: 10 },
          type: "ratelimit",
        },
        { name: "RELAY_ED25519_PRIVATE_KEY", type: "secret_text" },
        plain("RELAY_KEY_ID", "pilot-2026-07"),
        plain("WITSELF_EDGE_RELEASE_COMMIT", commit),
        plain("WITSELF_EDGE_RELEASE_DATE", date),
        plain("WITSELF_EDGE_RELEASE_VERSION", releaseVersion),
      ],
    },
  };
}

function fixtures() {
  return structuredClone({
    status: {
      id: deploymentID,
      strategy: "percentage",
      versions: [{ version_id: currentID, percentage: 100 }],
    },
    current: version({
      id: currentID,
      number: 42,
      releaseVersion: "2.0.0",
      commit: "c".repeat(40),
      date: "2026-08-09T08:00:00-06:00",
    }),
    candidate: version({
      id: candidateID,
      number: 39,
      releaseVersion: "1.9.0",
      commit: "d".repeat(40),
      date: "2026-08-08T14:00:00Z",
    }),
  });
}

function createPlan(fixture = fixtures()) {
  return createRollbackPlan(fixture.status, fixture.current, fixture.candidate, {
    now: () => new Date("2026-08-09T15:00:00Z"),
  });
}

function binding(candidate, name) {
  return candidate.resources.bindings.find((item) => item.name === name);
}

function removeBinding(candidate, name) {
  candidate.resources.bindings = candidate.resources.bindings.filter(
    (item) => item.name !== name,
  );
}

function operationsLease(events = []) {
  const controller = new AbortController();
  return {
    run: async (operation, work) => {
      events.push(`lease:${operation}`);
      assert.equal(operation, "email_edge_rollback");
      return work({
        signal: controller.signal,
        renew: async () => {
          events.push("lease:renew");
        },
      });
    },
  };
}

test("rollback planning produces a value-free exact-ID plan and SHA-256 fence", () => {
  const fixture = fixtures();
  const plan = createPlan(fixture);
  const { apply_fence: ignored, ...body } = plan;

  assert.equal(plan.current.deployment_id, deploymentID);
  assert.equal(plan.current.version_id, currentID);
  assert.equal(plan.candidate.version_id, candidateID);
  assert.equal(plan.apply_fence.sha256, sha256(canonicalJSON(body)));
  assert.equal(verifyPlan(plan, plan.apply_fence.sha256), plan.apply_fence.sha256);

  const encoded = JSON.stringify(plan);
  for (const hidden of [
    directoryID,
    publicKey,
    "https://self.witwave.ai/",
    "pilot-2026-07",
    "c".repeat(40),
    "d".repeat(40),
  ]) {
    assert.doesNotMatch(encoded, new RegExp(hidden.replaceAll("/", "\\/")));
  }
});

test("rollback planning refuses split traffic and a non-active current version", () => {
  {
    const fixture = fixtures();
    fixture.status.versions = [
      { version_id: currentID, percentage: 90 },
      { version_id: candidateID, percentage: 10 },
    ];
    assert.throws(() => createPlan(fixture), /one version at 100 percent/);
  }
  {
    const fixture = fixtures();
    fixture.status.versions[0].version_id = candidateID;
    assert.throws(() => createPlan(fixture), /did not match the active deployment/);
  }
});

test("rollback planning refuses handler, compatibility, storage, limiter, and flag drift", () => {
  const cases = [
    {
      mutate: (candidate) => { candidate.resources.script.handlers = []; },
      error: /email-only artifact/,
    },
    {
      mutate: (candidate) => { candidate.resources.script.handlers = ["email", "fetch"]; },
      error: /email-only artifact/,
    },
    {
      mutate: (candidate) => { candidate.resources.script_runtime.compatibility_date = "2026-08-01"; },
      error: /compatibility contract drifted/,
    },
    {
      mutate: (candidate) => { binding(candidate, "EMAIL_DIRECTORY").namespace_id = "a".repeat(32); },
      error: /operational contract drifted/,
    },
    {
      mutate: (candidate) => { binding(candidate, "EMAIL_EDGE_METRICS").dataset = "another_dataset"; },
      error: /metrics dataset binding drifted/,
    },
    {
      mutate: (candidate) => { binding(candidate, "REALM_ROUTE_COLD_MISS_LIMITER").simple.limit = 11; },
      error: /rate-limit binding REALM_ROUTE_COLD_MISS_LIMITER drifted/,
    },
    {
      mutate: (candidate) => { binding(candidate, "REALM_EMAIL_ALIAS_DELIVERY_ENABLED").text = "true"; },
      error: /operational contract drifted/,
    },
    {
      mutate: (candidate) => { binding(candidate, "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED").text = "TRUE"; },
      error: /not explicitly true or false/,
    },
  ];
  for (const item of cases) {
    const fixture = fixtures();
    item.mutate(fixture.candidate);
    assert.throws(() => createPlan(fixture), item.error);
  }
});

test("v0.0.240 contract is eligible only as a fully dark rollback candidate", () => {
  const legacy = fixtures();
  removeBinding(
    legacy.candidate,
    "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
  );
  const plan = createPlan(legacy);
  assert.equal(
    plan.checks.includes("legacy_managed_delivery_candidate_dark"),
    true,
  );

  {
    const activeCohort = fixtures();
    removeBinding(
      activeCohort.candidate,
      "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    );
    binding(
      activeCohort.current,
      "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    ).text = "acc_aaaaaaaaaaaaaaaa";
    assert.throws(
      () => createPlan(activeCohort),
      /requires an empty current cohort and both current delivery gates false/,
    );
  }

  {
    const activeGate = fixtures();
    removeBinding(
      activeGate.candidate,
      "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    );
    binding(
      activeGate.current,
      "REALM_EMAIL_ALIAS_DELIVERY_ENABLED",
    ).text = "true";
    assert.throws(
      () => createPlan(activeGate),
      /requires an empty current cohort and both current delivery gates false/,
    );
  }

  {
    const unsafeLegacy = fixtures();
    removeBinding(
      unsafeLegacy.candidate,
      "AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
    );
    binding(
      unsafeLegacy.candidate,
      "REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
    ).text = "true";
    assert.throws(
      () => createPlan(unsafeLegacy),
      /legacy managed-delivery contract is eligible only while fully dark/,
    );
  }
});

test("rollback planning refuses route-key changes and every custom-domain binding", () => {
  {
    const fixture = fixtures();
    binding(fixture.candidate, "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS").text =
      JSON.stringify({ "route-2026-09": publicKey });
    assert.throws(() => createPlan(fixture), /operational contract drifted/);
  }
  {
    const fixture = fixtures();
    binding(fixture.candidate, "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS").text = "{}";
    assert.throws(() => createPlan(fixture), /keyring was missing or invalid/);
  }
  {
    const fixture = fixtures();
    binding(fixture.candidate, "AGENT_EMAIL_ROUTE_ED25519_PUBLIC_KEYS").text =
      `{ "route-2026-08": "${publicKey}" }`;
    assert.throws(() => createPlan(fixture), /keyring was not canonical/);
  }
  for (const activation of [
    { name: "AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED", type: "secret_text" },
    plain("FUTURE_CUSTOM_DOMAIN_READY", "true"),
  ]) {
    const fixture = fixtures();
    fixture.candidate.resources.bindings.push(activation);
    assert.throws(() => createPlan(fixture), /custom-domain activation binding/);
  }
});

test("rollback planning requires immutable identity on both versions and an older candidate", () => {
  for (const target of ["current", "candidate"]) {
    for (const [name, value] of [
      ["WITSELF_EDGE_RELEASE_VERSION", ""],
      ["WITSELF_EDGE_RELEASE_COMMIT", "not-a-commit"],
      ["WITSELF_EDGE_RELEASE_DATE", "yesterday"],
    ]) {
      const fixture = fixtures();
      binding(fixture[target], name).text = value;
      assert.throws(() => createPlan(fixture), /immutable release identity/);
    }
  }
  {
    const fixture = fixtures();
    fixture.candidate.number = fixture.current.number;
    assert.throws(() => createPlan(fixture), /older distinct/);
  }
});

test("rollback planning requires the canonical production control-plane origin", () => {
  for (const target of ["current", "candidate"]) {
    const fixture = fixtures();
    binding(fixture[target], "CONTROL_PLANE_URL").text =
      "https://attacker-control.invalid/";
    assert.throws(
      () => createPlan(fixture),
      new RegExp(`${target} control-plane origin was not canonical`),
    );
  }
});

test("apply requires the exact reviewed plan and reverifies the final deployment", async () => {
  const fixture = fixtures();
  const plan = createPlan(fixture);
  let deployed = false;
  const deployCalls = [];
  const events = [];
  const receipt = await applyRollbackPlan(plan, plan.apply_fence.sha256, {
    loadStatus: async () => {
      events.push(deployed ? "status:after" : "status:before");
      return deployed ? {
        id: postDeploymentID,
        strategy: "percentage",
        versions: [{ version_id: candidateID, percentage: 100 }],
      } : structuredClone(fixture.status);
    },
    loadVersion: async (id) => {
      events.push(`version:${id}`);
      return structuredClone(id === currentID ? fixture.current : fixture.candidate);
    },
    deploy: async (id, { signal }) => {
      events.push(`deploy:${id}`);
      assert.equal(signal.aborted, false);
      deployCalls.push(id);
      deployed = true;
    },
    operationsLease: operationsLease(events),
  });

  assert.deepEqual(deployCalls, [candidateID]);
  assert.deepEqual(events, [
    "lease:email_edge_rollback",
    "status:before",
    `version:${currentID}`,
    `version:${candidateID}`,
    "lease:renew",
    `deploy:${candidateID}`,
    "lease:renew",
    "status:after",
    `version:${candidateID}`,
    "lease:renew",
  ]);
  assert.equal(receipt.outcome, "verified");
  assert.equal(receipt.plan_sha256, plan.apply_fence.sha256);
  assert.deepEqual(receipt.prior, {
    deployment_id: deploymentID,
    version_id: currentID,
  });
  assert.deepEqual(receipt.active, {
    deployment_id: postDeploymentID,
    version_id: candidateID,
  });
});

test("apply refuses a stale plan before mutation and detects failed post-verification", async () => {
  {
    const fixture = fixtures();
    const plan = createPlan(fixture);
    let deployCalls = 0;
    const stale = structuredClone(fixture.status);
    stale.id = postDeploymentID;
    await assert.rejects(() => applyRollbackPlan(plan, plan.apply_fence.sha256, {
      loadStatus: async () => stale,
      loadVersion: async (id) => id === currentID ? fixture.current : fixture.candidate,
      deploy: async () => { deployCalls += 1; },
      operationsLease: operationsLease(),
    }), /preconditions changed/);
    assert.equal(deployCalls, 0);
  }
  {
    const fixture = fixtures();
    const plan = createPlan(fixture);
    let calls = 0;
    await assert.rejects(() => applyRollbackPlan(plan, plan.apply_fence.sha256, {
      loadStatus: async () => {
        calls += 1;
        return calls === 1 ? fixture.status : {
          id: postDeploymentID,
          strategy: "percentage",
          versions: [{ version_id: currentID, percentage: 100 }],
        };
      },
      loadVersion: async (id) => id === currentID ? fixture.current : fixture.candidate,
      deploy: async () => {},
      operationsLease: operationsLease(),
    }), /without the candidate at 100 percent/);
  }
});

test("rollback lease collision refuses every provider read and mutation", async () => {
  const fixture = fixtures();
  const plan = createPlan(fixture);
  let reads = 0;
  let mutations = 0;
  const collision = new Error("another agent email operation already holds the lease");

  await assert.rejects(
    applyRollbackPlan(plan, plan.apply_fence.sha256, {
      loadStatus: async () => { reads += 1; },
      loadVersion: async () => { reads += 1; },
      deploy: async () => { mutations += 1; },
      operationsLease: {
        run: async (operation) => {
          assert.equal(operation, "email_edge_rollback");
          throw collision;
        },
      },
    }),
    (error) => error === collision,
  );
  assert.equal(reads, 0);
  assert.equal(mutations, 0);
});

test("rollback propagates lease loss to the active provider deployment", async () => {
  const fixture = fixtures();
  const plan = createPlan(fixture);
  const controller = new AbortController();
  let deploymentEntered;
  const entered = new Promise((resolve) => {
    deploymentEntered = resolve;
  });
  let statusReads = 0;

  await assert.rejects(
    applyRollbackPlan(plan, plan.apply_fence.sha256, {
      loadStatus: async () => {
        statusReads += 1;
        return structuredClone(fixture.status);
      },
      loadVersion: async (id) => structuredClone(
        id === currentID ? fixture.current : fixture.candidate,
      ),
      deploy: async (id, { signal }) => {
        assert.equal(id, candidateID);
        assert.equal(signal, controller.signal);
        deploymentEntered();
        await new Promise((resolve, reject) => {
          signal.addEventListener("abort", () => {
            reject(new Error("rollback deployment stopped after lease loss"));
          }, { once: true });
        });
      },
      operationsLease: {
        run: async (operation, work) => {
          assert.equal(operation, "email_edge_rollback");
          const pending = work({
            signal: controller.signal,
            renew: async () => {},
          });
          await entered;
          controller.abort(new Error("agent email operations lease renewal failed"));
          return pending;
        },
      },
    }),
    /stopped after lease loss/,
  );
  assert.equal(statusReads, 1);
});

test("production rollback lease acquisition ignores hostile endpoint environment", async () => {
  const requests = [];
  const hostileEnvironment = {
    CONTROL_PLANE_EDGE_TOKEN: "edge-token-at-least-16-characters",
    CONTROL_PLANE_URL: "https://attacker-control.invalid",
    WITSELF_CONTROL_PLANE: "https://attacker-witself.invalid",
  };
  const runtime = rollbackOperationsLeaseRuntime(hostileEnvironment, {
    fetchImpl: async (url, init) => {
      requests.push({ url, init });
      throw new Error("stop after observing the acquire request");
    },
    randomUUIDImpl: () => "11111111-1111-4111-8111-111111111111",
  });

  await assert.rejects(
    runtime.run("email_edge_rollback", async () => {}),
    /lease authority is unreachable/,
  );
  assert.equal(requests.length, 1);
  assert.equal(
    requests[0].url,
    "https://self.witwave.ai/v1/email/operations-lease:acquire",
  );
  assert.equal(requests[0].init.redirect, "error");
  assert.doesNotMatch(requests[0].url, /attacker/);
});

test("production rollback scrubs poisoned Wrangler reads and mutation", async () => {
  const calls = [];
  const environment = {
    PATH: "/safe/bin",
    CLOUDFLARE_ACCOUNT_ID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    CLOUDFLARE_API_TOKEN: "canonical-token",
    CF_ACCOUNT_ID: "wrong-account",
    CF_API_TOKEN: "wrong-token",
    CONTROL_PLANE_EDGE_TOKEN: "lease-only-secret-at-least-16-characters",
    CONTROL_PLANE_URL: "https://attacker.invalid",
    CLOUDFLARE_BASE_URL: "https://attacker.invalid",
    CLOUDFLARE_API_BASE_URL: "https://attacker.invalid",
    CLOUDFLARE_ENV: "staging",
    CF_API_BASE_URL: "https://attacker.invalid",
    DOTENV_KEY: "dotenv://attacker.invalid",
    WRANGLER_API_ENVIRONMENT: "staging",
    WRANGLER_LOG_PATH: "/tmp/unsafe.log",
    WRANGLER_OUTPUT_FILE_PATH: "/tmp/unsafe-output.json",
    WRANGLER_CI_OVERRIDE_NAME: "wrong-worker",
    WRANGLER_AUTH_URL: "https://attacker.invalid",
    NODE_OPTIONS: "--require attacker",
    NODE_DEBUG: "http",
    NODE_V8_COVERAGE: "/tmp/unsafe-coverage",
    SSLKEYLOGFILE: "/tmp/unsafe-tls",
    WITSELF_CONTROL_PLANE: "https://attacker.invalid",
  };
  const runtime = rollbackLiveRuntime(environment, {
    inspect: (args, env) => {
      calls.push({ type: "inspect", args, env });
      return {};
    },
    interactive: () => true,
    operationsLease: operationsLease(),
    runCommand: async (command, args, options) => {
      calls.push({ type: "mutation", command, args, env: options.env });
    },
  });
  await runtime.loadStatus();
  await runtime.loadVersion(candidateID);
  await runtime.deploy(candidateID);

  assert.deepEqual(calls[0].args, [
    "deployments", "status", "--name", "witself-agent-email-receive", "--json",
    "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
  ]);
  assert.deepEqual(calls[1].args, [
    "versions", "view", candidateID,
    "--name", "witself-agent-email-receive", "--json",
    "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
  ]);
  assert.equal(calls[2].command, "wrangler");
  assert.equal(calls[2].args.includes("witself-agent-email-receive"), true);
  for (const call of calls) {
    assert.equal(
      call.env.CLOUDFLARE_ACCOUNT_ID,
      PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    );
    assert.equal(call.env.CLOUDFLARE_API_TOKEN, "canonical-token");
    for (const unsafe of [
      "CF_ACCOUNT_ID",
      "CF_API_TOKEN",
      "CONTROL_PLANE_EDGE_TOKEN",
      "CONTROL_PLANE_URL",
      "CLOUDFLARE_BASE_URL",
      "CLOUDFLARE_API_BASE_URL",
      "CLOUDFLARE_ENV",
      "CF_API_BASE_URL",
      "DOTENV_KEY",
      "WRANGLER_API_ENVIRONMENT",
      "WRANGLER_LOG_PATH",
      "WRANGLER_OUTPUT_FILE_PATH",
      "WRANGLER_CI_OVERRIDE_NAME",
      "WRANGLER_AUTH_URL",
      "NODE_OPTIONS",
      "NODE_DEBUG",
      "NODE_V8_COVERAGE",
      "SSLKEYLOGFILE",
      "WITSELF_CONTROL_PLANE",
    ]) {
      assert.equal(Object.hasOwn(call.env, unsafe), false, unsafe);
    }
  }
});

test("production rollback refuses another Cloudflare account before inspection", () => {
  assert.throws(
    () => rollbackLiveRuntime({
      CLOUDFLARE_ACCOUNT_ID: "6236aa0c39cdd8d171deab7f86a12bc5",
      CLOUDFLARE_API_TOKEN: "canonical-token",
    }),
    /must identify production account/,
  );
});

test("plan hashing and CLI parsing fail closed", () => {
  const plan = createPlan();
  assert.throws(() => verifyPlan(plan, "0".repeat(64)), /did not match/);
  const tampered = structuredClone(plan);
  tampered.candidate.version_id = "cccccccc-dddd-4eee-8fff-000000000000";
  assert.throws(() => verifyPlan(tampered, plan.apply_fence.sha256), /fence did not match/);

  assert.deepEqual(parseArgs(["--candidate-version", candidateID]), {
    apply: false,
    candidateVersion: candidateID,
    output: "",
    plan: "",
    planSHA256: "",
  });
  assert.deepEqual(parseArgs([
    "--apply", "--plan", "rollback.json", "--plan-sha256", "a".repeat(64),
  ]), {
    apply: true,
    candidateVersion: "",
    output: "",
    plan: "rollback.json",
    planSHA256: "a".repeat(64),
  });
  assert.throws(() => parseArgs(["--apply", "--plan", "rollback.json"]), /requires only/);
  assert.throws(() => parseArgs([]), /planning requires/);

  const deploymentArguments = rollbackDeploymentArguments(candidateID);
  assert.deepEqual(deploymentArguments, [
    "versions", "deploy", `${candidateID}@100`,
    "--name", "witself-agent-email-receive",
    "--message", `Guarded rollback to ${candidateID}`,
    "--env-file", WRANGLER_PRODUCTION_ENV_FILE,
  ]);
  assert.equal(deploymentArguments.includes("--yes"), false);
});
