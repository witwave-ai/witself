import assert from "node:assert/strict";
import {
  chmodSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  assertBillingRolloutSourceFenceBracket,
  LIFECYCLE_IN_FLIGHT_BOUND_MS,
  observeBillingRolloutSourceFence,
  main as sourceFenceMain,
  parseBillingRolloutSourceFenceArgs,
  readBillingRolloutSourceFenceAttestationFile,
  reviewedBillingRolloutConfigIdentity,
} from "../scripts/billing-rollout-source-fence.mjs";
import {
  PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
} from "../../agent-email/scripts/wrangler-environment.mjs";

const deploymentID = "11111111-1111-4111-8111-111111111111";
const workerVersionID = "22222222-2222-4222-8222-222222222222";
const applicationID = "33333333-3333-4333-8333-333333333333";
const namespaceID = "4".repeat(32);
const targetVersion = 18;
const releaseVersion = "0.0.255";
const releaseCommit = "c".repeat(40);
const releaseDate = "2026-08-17T20:00:00Z";
const imageDigest = `sha256:${"b".repeat(64)}`;
const reviewedConfigSHA256 = "d".repeat(64);
const disabledAt = "2026-08-17T22:05:00Z";
const observedAt = "2026-08-17T22:10:00Z";

function canonicalJSON(value) {
  if (value === null || typeof value === "boolean" ||
      typeof value === "string" || typeof value === "number") {
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map(canonicalJSON).join(",")}]`;
  }
  return `{${Object.keys(value).sort().map((key) =>
    `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(",")}}`;
}

function clockAt(value) {
  return () => new Date(value);
}

function deployment(id = deploymentID, versionID = workerVersionID) {
  return {
    id,
    strategy: "percentage",
    versions: [{ version_id: versionID, percentage: 100 }],
  };
}

function secretBinding(name) {
  return { name, type: "secret_text" };
}

function workerVersion(extraSecrets = []) {
  return {
    id: workerVersionID,
    metadata: { source: "wrangler" },
    resources: {
      script: {
        etag: "a".repeat(64),
        handlers: ["fetch", "scheduled"],
        named_handlers: [
          { name: "Backend", handlers: ["class"] },
          { name: "AccountLifecycle", handlers: ["class"] },
        ],
      },
      script_runtime: {
        containers: [{ class_name: "Backend" }],
      },
      bindings: [
        {
          name: "CONTROL_PLANE",
          type: "durable_object_namespace",
          class_name: "Backend",
          namespace_id: namespaceID,
        },
        {
          name: "WITSELF_EDGE_RELEASE_VERSION",
          type: "plain_text",
          text: releaseVersion,
        },
        {
          name: "WITSELF_EDGE_RELEASE_COMMIT",
          type: "plain_text",
          text: releaseCommit,
        },
        {
          name: "WITSELF_EDGE_RELEASE_DATE",
          type: "plain_text",
          text: releaseDate,
        },
        secretBinding("CONTROL_PLANE_EDGE_TOKEN"),
        ...extraSecrets.map(secretBinding),
      ],
    },
  };
}

function secrets(extra = []) {
  return ["CONTROL_PLANE_EDGE_TOKEN", ...extra]
    .sort()
    .map(secretBinding);
}

function application({
  version = targetVersion,
  digest = imageDigest,
  maxInstances = 2,
  id = applicationID,
} = {}) {
  return {
    id,
    name: "witself-control-plane",
    version,
    max_instances: maxInstances,
    durable_objects: { namespace_id: namespaceID },
    scheduling_policy: "default",
    configuration: {
      image: `registry.cloudflare.example/witself@${digest}`,
      instance_type: "lite",
    },
  };
}

function instance({
  id = "55555555-5555-4555-8555-555555555555",
  name = "singleton-private-name",
  state = "running",
  version = targetVersion,
  location = "private-location",
  created = "2026-08-17T21:00:00Z",
} = {}) {
  return { id, name, state, location, version, created };
}

function inactiveInstance(overrides = {}) {
  return instance({
    state: "inactive",
    version: null,
    location: null,
    ...overrides,
  });
}

function instancePage(rows, pageToken = null, nextPageToken = null) {
  return {
    instances: rows,
    result_info: {
      per_page: 100,
      page_token: pageToken,
      next_page_token: nextPageToken,
    },
  };
}

function fixtures({
  worker = workerVersion(),
  persistentSecrets = secrets(),
  app = application(),
  scans = [[], []],
  deployments = [deployment(), deployment(), deployment()],
  applications = [app, app],
  workers = [worker, worker],
  secretScans = [persistentSecrets, persistentSecrets],
} = {}) {
  const queues = {
    deployments: [...deployments],
    workers: [...workers],
    secrets: [...secretScans],
    applications: [...applications],
    scans: scans.map((rows) => [instancePage(rows)]),
  };
  const calls = [];
  const inspect = (args) => {
    calls.push([...args]);
    if (args[0] === "deployments") return queues.deployments.shift();
    if (args[0] === "versions") return queues.workers.shift();
    if (args[0] === "secret") return queues.secrets.shift();
    if (args[0] === "containers" && args[1] === "info") {
      return queues.applications.shift();
    }
    if (args[0] === "containers" && args[1] === "instances") {
      const scan = queues.scans[0];
      const page = scan.shift();
      if (scan.length === 0) queues.scans.shift();
      return page;
    }
    throw new Error(`unexpected inspection ${args[0]} ${args[1]}`);
  };
  return { inspect, calls };
}

function options(overrides = {}) {
  return {
    config: "/private/wrangler.generated.jsonc",
    expectedAccountID: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    expectedTargetApplicationID: applicationID,
    expectedTargetApplicationVersion: targetVersion,
    expectedTargetImageDigest: imageDigest,
    expectedTargetReleaseVersion: releaseVersion,
    expectedTargetReleaseCommit: releaseCommit,
    reviewedConfigIdentity: {
      account_id: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
      config_sha256: reviewedConfigSHA256,
      target_release: {
        version: releaseVersion,
        commit: releaseCommit,
        date: releaseDate,
      },
    },
    priorLifecycleDisabledAttestation: null,
    ...overrides,
  };
}

function observe({
  optionOverrides = {},
  fixtureOverrides = {},
  at = observedAt,
} = {}) {
  const provider = fixtures(fixtureOverrides);
  return {
    attestation: observeBillingRolloutSourceFence(
      options(optionOverrides),
      provider.inspect,
      clockAt(at),
    ),
    ...provider,
  };
}

function initialAttestation(overrides = {}) {
  return observe({ at: disabledAt, ...overrides }).attestation;
}

function matureAttestation(prior, overrides = {}) {
  return observe({
    at: observedAt,
    optionOverrides: { priorLifecycleDisabledAttestation: prior },
    ...overrides,
  }).attestation;
}

test("emits a private exact-target initial attestation without raw provider data", () => {
  const row = inactiveInstance();
  const { attestation, calls } = observe({
    at: disabledAt,
    fixtureOverrides: { scans: [[row], [row]] },
  });

  assert.equal(attestation.schema, "witself.billing-rollout-source-fence.v1");
  assert.equal(attestation.observed_at, disabledAt);
  assert.equal(attestation.billing_mutation_cohort_accounts, 0);
  assert.deepEqual(attestation.source_fleet, {
    api_replicas: 0,
    reconciler_replicas: 1,
  });
  assert.deepEqual({
    account: attestation.fence.cloudflare_account_id,
    deployment: attestation.fence.worker_deployment_id,
    worker: attestation.fence.worker_version_id,
    namespace: attestation.fence.backend_namespace_id,
    application: attestation.fence.container_application_id,
    application_version: attestation.fence.container_application_version,
    target_current: attestation.fence.target_application_current,
    instances: attestation.fence.container_instance_count,
    writers: attestation.fence.potential_writer_instance_count,
    prior: attestation.fence.prior_lifecycle_disabled_inspection_sha256,
  }, {
    account: PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    deployment: deploymentID,
    worker: workerVersionID,
    namespace: namespaceID,
    application: applicationID,
    application_version: targetVersion,
    target_current: true,
    instances: 1,
    writers: 0,
    prior: null,
  });
  assert.match(attestation.inspection_sha256, /^[0-9a-f]{64}$/);
  for (const field of [
    "binding_inventory_sha256",
    "secret_name_inventory_sha256",
    "container_application_sha256",
    "source_instance_inventory_sha256",
    "reviewed_config_sha256",
  ]) assert.match(attestation.fence[field], /^[0-9a-f]{64}$/);

  const output = JSON.stringify(attestation);
  assert.equal(output.includes("singleton-private-name"), false);
  assert.equal(output.includes("55555555-5555-4555-8555-555555555555"), false);
  assert.equal(output.includes("CONTROL_PLANE_EDGE_TOKEN"), false);
  assert.equal(output.includes("registry.cloudflare.example"), false);
  assert.equal(calls.filter((args) => args[0] === "deployments").length, 3);
  assert.equal(calls.filter((args) => args[0] === "versions").length, 2);
  assert.equal(calls.filter((args) => args[0] === "secret").length, 2);
  assert.equal(calls.filter((args) => args[1] === "info").length, 2);
  assert.equal(calls.filter((args) => args[1] === "instances").length, 2);
});

test("a self-hashed prior attestation proves the four-minute reconciler drain", () => {
  const prior = initialAttestation();
  const mature = matureAttestation(prior);

  assert.deepEqual(mature.source_fleet, {
    api_replicas: 0,
    reconciler_replicas: 0,
  });
  assert.equal(mature.fence.lifecycle_disabled_observed_at, disabledAt);
  assert.equal(
    mature.fence.prior_lifecycle_disabled_inspection_sha256,
    prior.inspection_sha256,
  );
  assert.notEqual(mature.inspection_sha256, prior.inspection_sha256);
  assert.equal(LIFECYCLE_IN_FLIGHT_BOUND_MS, 240_000);

  const young = observe({
    at: "2026-08-17T22:08:59Z",
    optionOverrides: { priorLifecycleDisabledAttestation: prior },
  }).attestation;
  assert.deepEqual(young.source_fleet, {
    api_replicas: 0,
    reconciler_replicas: 1,
  });
  const exact = observe({
    at: "2026-08-17T22:09:00Z",
    optionOverrides: { priorLifecycleDisabledAttestation: prior },
  }).attestation;
  assert.equal(exact.source_fleet.reconciler_replicas, 0);
});

test("canonical hashes ignore JSON key, binding, secret, and row order", () => {
  const reorderedWorker = workerVersion();
  reorderedWorker.resources.bindings = reorderedWorker.resources.bindings
    .reverse()
    .map((binding) => Object.fromEntries(Object.entries(binding).reverse()));
  const reorderedApplication = Object.fromEntries(
    Object.entries(application()).reverse(),
  );
  const firstRow = inactiveInstance();
  const secondRow = inactiveInstance({
    id: "66666666-6666-4666-8666-666666666666",
  });
  const first = observe({
    at: disabledAt,
    fixtureOverrides: {
      scans: [[firstRow, secondRow], [firstRow, secondRow]],
    },
  }).attestation;
  const second = observe({
    at: disabledAt,
    fixtureOverrides: {
      worker: reorderedWorker,
      persistentSecrets: secrets().reverse(),
      app: reorderedApplication,
      scans: [[secondRow, firstRow], [secondRow, firstRow]],
    },
  }).attestation;
  assert.equal(second.inspection_sha256, first.inspection_sha256);
  assert.equal(
    second.fence.binding_inventory_sha256,
    first.fence.binding_inventory_sha256,
  );
  assert.equal(
    second.fence.container_application_sha256,
    first.fence.container_application_sha256,
  );
  assert.equal(
    second.fence.source_instance_inventory_sha256,
    first.fence.source_instance_inventory_sha256,
  );
});

test("any target application mismatch synthesizes a positive source", () => {
  for (const app of [
    application({ version: targetVersion - 1 }),
    application({ digest: `sha256:${"e".repeat(64)}` }),
    application({ maxInstances: 3 }),
  ]) {
    const attestation = observe({ fixtureOverrides: { app } }).attestation;
    assert.equal(attestation.fence.target_application_current, false);
    assert.deepEqual(attestation.source_fleet, {
      api_replicas: 1,
      reconciler_replicas: 1,
    });
  }
});

test("every non-null instance version is a writer, including stopped target rows", () => {
  const rows = [
    instance({ state: "stopped" }),
    instance({
      id: "66666666-6666-4666-8666-666666666666",
      state: "unknown",
    }),
    instance({
      id: "77777777-7777-4777-8777-777777777777",
      version: targetVersion - 1,
      state: "failed",
    }),
    inactiveInstance({
      id: "88888888-8888-4888-8888-888888888888",
    }),
  ];
  const attestation = observe({
    fixtureOverrides: { scans: [rows, rows] },
  }).attestation;
  assert.deepEqual(attestation.source_fleet, {
    api_replicas: 3,
    reconciler_replicas: 3,
  });
  assert.equal(attestation.fence.container_instance_count, 4);
  assert.equal(attestation.fence.target_version_instance_count, 2);
  assert.equal(attestation.fence.incompatible_instance_count, 1);
  assert.equal(attestation.fence.potential_writer_instance_count, 3);
});

test("only inactive/version-null rows are accepted as non-writers", () => {
  const ambiguous = instance({ state: "running", version: null });
  assert.throws(
    () => observe({ fixtureOverrides: { scans: [[ambiguous], [ambiguous]] } }),
    /application version/,
  );
  const inconsistent = inactiveInstance({ version: targetVersion });
  assert.throws(
    () => observe({
      fixtureOverrides: { scans: [[inconsistent], [inconsistent]] },
    }),
    /inactive container instance inventory was ambiguous/,
  );
});

test("a prior attestation must itself prove zero writers and an absent gate", () => {
  const writer = instance({ state: "stopped" });
  const writerPrior = initialAttestation({
    fixtureOverrides: { scans: [[writer], [writer]] },
  });
  assert.throws(
    () => matureAttestation(writerPrior),
    /did not prove the exact stopped target/,
  );

  const gatedWorker = workerVersion(["CP_PLAN_LIFECYCLE_ENABLED"]);
  const gatedPrior = initialAttestation({
    fixtureOverrides: {
      worker: gatedWorker,
      persistentSecrets: secrets(["CP_PLAN_LIFECYCLE_ENABLED"]),
    },
  });
  assert.throws(
    () => matureAttestation(gatedPrior),
    /did not prove the exact stopped target/,
  );
});

test("a prior hash cannot hide tampering or stable Worker/application drift", () => {
  const prior = initialAttestation();
  const tampered = structuredClone(prior);
  tampered.fence.worker_script_etag = "f".repeat(64);
  assert.throws(
    () => matureAttestation(tampered),
    /self hash did not match/,
  );

  const alternateDeploymentID = "99999999-9999-4999-8999-999999999999";
  const alternatePrior = initialAttestation({
    fixtureOverrides: {
      deployments: [
        deployment(alternateDeploymentID),
        deployment(alternateDeploymentID),
        deployment(alternateDeploymentID),
      ],
    },
  });
  assert.throws(
    () => matureAttestation(alternatePrior),
    /source identity changed since the prior/,
  );
});

test("billing cohort, release binding, secret, and namespace ambiguity fail closed", () => {
  const cohortWorker = workerVersion(["CP_BILLING_ACCOUNT_ALLOWLIST"]);
  assert.throws(
    () => observe({
      fixtureOverrides: {
        worker: cohortWorker,
        persistentSecrets: secrets(["CP_BILLING_ACCOUNT_ALLOWLIST"]),
      },
    }),
    /zero-account cohort cannot be proven/,
  );

  const wrongRelease = workerVersion();
  wrongRelease.resources.bindings.find((binding) =>
    binding.name === "WITSELF_EDGE_RELEASE_COMMIT").text = "e".repeat(40);
  assert.throws(
    () => observe({ fixtureOverrides: { worker: wrongRelease } }),
    /release identity was invalid/,
  );

  assert.throws(
    () => observe({ fixtureOverrides: { persistentSecrets: [] } }),
    /secret inventories disagreed/,
  );

  const wrongNamespace = application();
  wrongNamespace.durable_objects.namespace_id = "9".repeat(32);
  assert.throws(
    () => observe({ fixtureOverrides: { app: wrongNamespace } }),
    /did not match the active Backend namespace/,
  );
});

test("deployment, application, secret, and instance drift during one observation fail", () => {
  assert.throws(
    () => observe({
      fixtureOverrides: {
        deployments: [
          deployment(),
          deployment("88888888-8888-4888-8888-888888888888"),
        ],
      },
    }),
    /source state changed/,
  );
  assert.throws(
    () => observe({
      fixtureOverrides: {
        deployments: [
          deployment(),
          deployment(),
          deployment("88888888-8888-4888-8888-888888888888"),
        ],
      },
    }),
    /deployment changed after exact provider inspection/,
  );
  assert.throws(
    () => observe({
      fixtureOverrides: {
        applications: [application(), application({ maxInstances: 3 })],
      },
    }),
    /source state changed/,
  );
  assert.throws(
    () => observe({
      fixtureOverrides: {
        secretScans: [secrets(), secrets(["CP_PLAN_LIFECYCLE_ENABLED"])],
        workers: [
          workerVersion(),
          workerVersion(["CP_PLAN_LIFECYCLE_ENABLED"]),
        ],
      },
    }),
    /source state changed/,
  );
  assert.throws(
    () => observe({
      fixtureOverrides: {
        scans: [[instance()], [instance({ state: "stopping" })]],
      },
    }),
    /source state changed/,
  );
});

test("instance inventory follows and fences every explicit JSON page", () => {
  const { inspect: baseInspect, calls } = fixtures({ scans: [[], []] });
  const first = inactiveInstance();
  const second = inactiveInstance({
    id: "66666666-6666-4666-8666-666666666666",
  });
  let pageCall = 0;
  const inspect = (args, operation) => {
    if (args[0] !== "containers" || args[1] !== "instances") {
      return baseInspect(args, operation);
    }
    calls.push([...args]);
    const continued = pageCall % 2 === 1;
    pageCall += 1;
    return continued
      ? instancePage([second], "next-private-page", null)
      : instancePage([first], null, "next-private-page");
  };
  const attestation = observeBillingRolloutSourceFence(
    options(), inspect, clockAt(disabledAt),
  );
  assert.equal(attestation.fence.container_instance_count, 2);
  assert.equal(attestation.fence.potential_writer_instance_count, 0);
  assert.equal(pageCall, 4);
  assert.equal(calls.filter((args) =>
    args[0] === "containers" && args[1] === "instances" &&
    args.includes("--page-token")).length, 2);
  assert.equal(JSON.stringify(attestation).includes("next-private-page"), false);

  const { inspect: ordinaryInspect } = fixtures();
  const badInspect = (args, operation) => {
    if (args[0] === "containers" && args[1] === "instances") {
      return instancePage([], null, "unexpected-continuation");
    }
    return ordinaryInspect(args, operation);
  };
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(), badInspect, clockAt(disabledAt),
    ),
    /continuation fence/,
  );
});

test("two zero-writer observations strictly bracket inventory despite inactive churn", () => {
  const prior = initialAttestation();
  const before = matureAttestation(prior);
  const row = inactiveInstance();
  const after = observe({
    at: "2026-08-17T22:11:00Z",
    optionOverrides: { priorLifecycleDisabledAttestation: prior },
    fixtureOverrides: { scans: [[row], [row]] },
  }).attestation;
  const bracket = assertBillingRolloutSourceFenceBracket(before, after);
  assert.equal(bracket.pre_inspection_sha256, before.inspection_sha256);
  assert.equal(bracket.post_inspection_sha256, after.inspection_sha256);
  assert.match(bracket.bracket_sha256, /^[0-9a-f]{64}$/);
  assert.notEqual(
    before.fence.source_instance_inventory_sha256,
    after.fence.source_instance_inventory_sha256,
  );

  assert.throws(
    () => assertBillingRolloutSourceFenceBracket(before, before),
    /did not remain exactly stopped/,
  );
  const writer = instance({ state: "stopped" });
  const unsafeAfter = observe({
    at: "2026-08-17T22:11:00Z",
    optionOverrides: { priorLifecycleDisabledAttestation: prior },
    fixtureOverrides: { scans: [[writer], [writer]] },
  }).attestation;
  assert.throws(
    () => assertBillingRolloutSourceFenceBracket(before, unsafeAfter),
    /did not remain exactly stopped/,
  );

  const alternateDeploymentID = "99999999-9999-4999-8999-999999999999";
  const alternatePrior = initialAttestation({
    fixtureOverrides: {
      deployments: [
        deployment(alternateDeploymentID),
        deployment(alternateDeploymentID),
        deployment(alternateDeploymentID),
      ],
    },
  });
  const alternateAfter = matureAttestation(alternatePrior, {
    at: "2026-08-17T22:11:00Z",
    fixtureOverrides: {
      deployments: [
        deployment(alternateDeploymentID),
        deployment(alternateDeploymentID),
        deployment(alternateDeploymentID),
      ],
    },
  });
  assert.throws(
    () => assertBillingRolloutSourceFenceBracket(before, alternateAfter),
    /did not remain exactly stopped/,
  );
});

test("strict prior files require canonical self-hashed 0600 content and real paths", (t) => {
  const temporary = realpathSync(mkdtempSync(join(tmpdir(), "source-fence-")));
  t.after(() => rmSync(temporary, { recursive: true, force: true }));
  const prior = initialAttestation();
  const path = join(temporary, "prior.json");
  writeFileSync(path, `${canonicalJSON(prior)}\n`, { mode: 0o600 });
  chmodSync(path, 0o600);
  assert.deepEqual(
    readBillingRolloutSourceFenceAttestationFile(path),
    prior,
  );

  chmodSync(path, 0o644);
  assert.throws(
    () => readBillingRolloutSourceFenceAttestationFile(path),
    /exact private regular file/,
  );
  chmodSync(path, 0o600);
  writeFileSync(path, `${JSON.stringify(prior, null, 2)}\n`);
  assert.throws(
    () => readBillingRolloutSourceFenceAttestationFile(path),
    /one canonical JSON line/,
  );

  writeFileSync(path, `${canonicalJSON(prior)}\n`);
  const link = join(temporary, "prior-link.json");
  symlinkSync(path, link);
  assert.throws(
    () => readBillingRolloutSourceFenceAttestationFile(link),
    /symbolic link/,
  );
});

test("reviewed config identity binds the exact production account and release", () => {
  let source = readFileSync(
    new URL("../wrangler.template.jsonc", import.meta.url),
    "utf8",
  );
  for (const [placeholder, replacement] of [
    ["__WITSELF_VERSION__", releaseVersion],
    ["__WITSELF_COMMIT__", releaseCommit],
    ["__WITSELF_DATE__", releaseDate],
    ["__WITSELF_EDGE_RELEASE_VERSION__", releaseVersion],
    ["__WITSELF_EDGE_RELEASE_COMMIT__", releaseCommit],
    ["__WITSELF_EDGE_RELEASE_DATE__", releaseDate],
    ["__EMAIL_DIRECTORY_KV_ID__", "e".repeat(32)],
    ["__AGENT_EMAIL_ROUTE_SIGNING_KEY_ID__", "route-2026-08"],
    ["__CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST__", ""],
  ]) source = source.replace(placeholder, replacement);

  const identity = reviewedBillingRolloutConfigIdentity(
    source,
    "src/index.js",
    PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    releaseVersion,
    releaseCommit,
  );
  assert.equal(identity.account_id, PRODUCTION_CLOUDFLARE_ACCOUNT_ID);
  assert.match(identity.config_sha256, /^[0-9a-f]{64}$/);
  assert.deepEqual(identity.target_release, {
    version: releaseVersion,
    commit: releaseCommit,
    date: releaseDate,
  });
  assert.throws(
    () => reviewedBillingRolloutConfigIdentity(
      source.replace(
        "{",
        `{\n  "account_id": "${PRODUCTION_CLOUDFLARE_ACCOUNT_ID}",`,
      ),
      "src/index.js",
      PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
      releaseVersion,
      releaseCommit,
    ),
    /top-level contract/,
  );
  assert.throws(
    () => reviewedBillingRolloutConfigIdentity(
      source,
      "src/index.js",
      PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
      "0.0.254",
      releaseCommit,
    ),
    /explicit target release identity/,
  );
});

test("strict CLI arguments bind config, account, target image, app, and release", (t) => {
  const required = [
    "--config", "/private/wrangler.generated.jsonc",
    "--expected-account-id", PRODUCTION_CLOUDFLARE_ACCOUNT_ID,
    "--expected-target-application-id", applicationID,
    "--expected-target-application-version", String(targetVersion),
    "--expected-target-image-digest", imageDigest,
    "--expected-target-release-version", releaseVersion,
    "--expected-target-release-commit", releaseCommit,
  ];
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([]),
    /--config is required/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      ...required.slice(0, 7), "17x", ...required.slice(8),
    ]),
    /canonical positive integer/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      ...required,
      "--expected-target-application-id", applicationID,
    ]),
    /duplicate argument/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      ...required.slice(0, 3), "0".repeat(32), ...required.slice(4),
    ]),
    /expected-account-id/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      "--config", "relative.jsonc", ...required.slice(2),
    ]),
    /normalized absolute path/,
  );

  const priorPath = "/private/prior-source-fence.json";
  const parsed = parseBillingRolloutSourceFenceArgs([
    ...required,
    "--prior-lifecycle-disabled-attestation", priorPath,
    "--wrangler-cwd", "/private/wrangler",
    "--reviewed-env-file", "/private/wrangler.env",
  ]);
  assert.equal(parsed.config, "/private/wrangler.generated.jsonc");
  assert.equal(parsed.expectedAccountID, PRODUCTION_CLOUDFLARE_ACCOUNT_ID);
  assert.equal(parsed.expectedTargetApplicationID, applicationID);
  assert.equal(parsed.expectedTargetApplicationVersion, targetVersion);
  assert.equal(parsed.expectedTargetImageDigest, imageDigest);
  assert.equal(parsed.expectedTargetReleaseVersion, releaseVersion);
  assert.equal(parsed.expectedTargetReleaseCommit, releaseCommit);
  assert.equal(parsed.priorLifecycleDisabledAttestationPath, priorPath);

  const nonRFCApplicationID = "aaaaaaaa-bbbb-0000-0000-cccccccccccc";
  const nonRFC = parseBillingRolloutSourceFenceArgs(required.map((value) =>
    value === applicationID ? nonRFCApplicationID : value));
  assert.equal(nonRFC.expectedTargetApplicationID, nonRFCApplicationID);

  const mutableDirectory = realpathSync(mkdtempSync(join(
    tmpdir(),
    "mutable-source-fence-config-",
  )));
  t.after(() => rmSync(mutableDirectory, { recursive: true, force: true }));
  const mutableConfig = join(mutableDirectory, "wrangler.generated.jsonc");
  writeFileSync(mutableConfig, "{}\n", { mode: 0o600 });
  chmodSync(mutableConfig, 0o600);
  assert.throws(
    () => sourceFenceMain(required.map((value) =>
      value === "/private/wrangler.generated.jsonc"
        ? mutableConfig
        : value)),
    /exact private control-plane configuration path/,
  );
});
