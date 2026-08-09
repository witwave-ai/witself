import assert from "node:assert/strict";
import test from "node:test";

import {
  applyRollbackPlan,
  canonicalJSON,
  createRollbackPlan,
  parseArgs,
  rollbackDeploymentArguments,
  sha256,
  verifyPlan,
} from "../scripts/rollback.mjs";

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

test("apply requires the exact reviewed plan and reverifies the final deployment", async () => {
  const fixture = fixtures();
  const plan = createPlan(fixture);
  let deployed = false;
  const deployCalls = [];
  const receipt = await applyRollbackPlan(plan, plan.apply_fence.sha256, {
    loadStatus: async () => deployed ? {
      id: postDeploymentID,
      strategy: "percentage",
      versions: [{ version_id: candidateID, percentage: 100 }],
    } : structuredClone(fixture.status),
    loadVersion: async (id) => structuredClone(
      id === currentID ? fixture.current : fixture.candidate,
    ),
    deploy: async (id) => {
      deployCalls.push(id);
      deployed = true;
    },
  });

  assert.deepEqual(deployCalls, [candidateID]);
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
    }), /without the candidate at 100 percent/);
  }
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
    "--name", "witself-agent-email-pilot",
    "--message", `Guarded rollback to ${candidateID}`,
  ]);
  assert.equal(deploymentArguments.includes("--yes"), false);
});
