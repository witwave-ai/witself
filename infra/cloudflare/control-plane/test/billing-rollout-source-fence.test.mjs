import assert from "node:assert/strict";
import test from "node:test";

import {
  LIFECYCLE_IN_FLIGHT_BOUND_MS,
  observeBillingRolloutSourceFence,
  parseBillingRolloutSourceFenceArgs,
} from "../scripts/billing-rollout-source-fence.mjs";

const deploymentID = "11111111-1111-4111-8111-111111111111";
const workerVersionID = "22222222-2222-4222-8222-222222222222";
const applicationID = "33333333-3333-4333-8333-333333333333";
const namespaceID = "4".repeat(32);
const sourceVersion = 17;
const targetVersion = 18;
const observedAt = "2026-08-17T22:10:00Z";
const disabledAt = "2026-08-17T22:05:00Z";

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
          text: "0.0.255",
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

function application(version = targetVersion) {
  return {
    id: applicationID,
    name: "witself-control-plane",
    version,
    max_instances: 2,
    durable_objects: { namespace_id: namespaceID },
    configuration: {
      image: "registry.cloudflare.example/witself@sha256:" + "b".repeat(64),
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
  scans = [[instance()], [instance()]],
  deployments = [deployment(), deployment()],
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
    expectedSourceApplicationID: applicationID,
    expectedSourceApplicationVersion: sourceVersion,
    lifecycleDisabledObservedAt: disabledAt,
    ...overrides,
  };
}

const clock = () => new Date(observedAt);

test("emits a private zero-source count fence without rows or secret values", () => {
  const { inspect, calls } = fixtures();
  const attestation = observeBillingRolloutSourceFence(
    options(),
    inspect,
    clock,
  );

  assert.equal(attestation.schema, "witself.billing-rollout-source-fence.v1");
  assert.equal(attestation.observed_at, observedAt);
  assert.equal(attestation.billing_mutation_cohort_accounts, 0);
  assert.deepEqual(attestation.source_fleet, {
    api_replicas: 0,
    reconciler_replicas: 0,
  });
  assert.deepEqual({
    worker_deployment_id: attestation.fence.worker_deployment_id,
    worker_version_id: attestation.fence.worker_version_id,
    worker_script_etag: attestation.fence.worker_script_etag,
    backend_namespace_id: attestation.fence.backend_namespace_id,
    container_application_id: attestation.fence.container_application_id,
    container_application_version:
      attestation.fence.container_application_version,
  }, {
    worker_deployment_id: deploymentID,
    worker_version_id: workerVersionID,
    worker_script_etag: "a".repeat(64),
    backend_namespace_id: namespaceID,
    container_application_id: applicationID,
    container_application_version: targetVersion,
  });
  assert.match(attestation.inspection_sha256, /^[0-9a-f]{64}$/);
  for (const field of [
    "binding_inventory_sha256",
    "secret_name_inventory_sha256",
    "container_application_sha256",
    "source_instance_inventory_sha256",
  ]) assert.match(attestation.fence[field], /^[0-9a-f]{64}$/);

  const output = JSON.stringify(attestation);
  assert.equal(output.includes("singleton-private-name"), false);
  assert.equal(output.includes("private-location"), false);
  assert.equal(output.includes("55555555-5555-4555-8555-555555555555"), false);
  assert.equal(output.includes("CONTROL_PLANE_EDGE_TOKEN"), false);
  assert.equal(output.includes("registry.cloudflare.example"), false);
  assert.equal(calls.filter((args) => args[0] === "deployments").length, 2);
  assert.equal(calls.filter((args) => args[0] === "versions").length, 2);
  assert.equal(calls.filter((args) => args[0] === "secret").length, 2);
  assert.equal(calls.filter((args) => args[1] === "info").length, 2);
  assert.equal(calls.filter((args) => args[1] === "instances").length, 2);
});

test("canonical hashes are independent of binding key and list order", () => {
  const reordered = workerVersion();
  reordered.resources.bindings = reordered.resources.bindings
    .reverse()
    .map((binding) => Object.fromEntries(Object.entries(binding).reverse()));
  const first = observeBillingRolloutSourceFence(
    options(), fixtures().inspect, clock,
  );
  const second = observeBillingRolloutSourceFence(
    options(), fixtures({
      worker: reordered,
      persistentSecrets: secrets().reverse(),
    }).inspect, clock,
  );
  assert.equal(
    second.fence.binding_inventory_sha256,
    first.fence.binding_inventory_sha256,
  );
  assert.equal(
    second.fence.secret_name_inventory_sha256,
    first.fence.secret_name_inventory_sha256,
  );
  assert.equal(second.inspection_sha256, first.inspection_sha256);

  const rowA = instance();
  const rowB = instance({
    id: "66666666-6666-4666-8666-666666666666",
  });
  const ordered = observeBillingRolloutSourceFence(
    options(), fixtures({ scans: [[rowA, rowB], [rowA, rowB]] }).inspect, clock,
  );
  const reversed = observeBillingRolloutSourceFence(
    options(), fixtures({ scans: [[rowB, rowA], [rowB, rowA]] }).inspect, clock,
  );
  assert.equal(
    reversed.fence.source_instance_inventory_sha256,
    ordered.fence.source_instance_inventory_sha256,
  );
  assert.equal(reversed.inspection_sha256, ordered.inspection_sha256);
});

test("a scale-to-zero source application remains a positive source fleet", () => {
  const attestation = observeBillingRolloutSourceFence(
    options(),
    fixtures({ app: application(sourceVersion), scans: [[], []] }).inspect,
    clock,
  );
  assert.equal(attestation.fence.source_application_spawnable, true);
  assert.deepEqual(attestation.source_fleet, {
    api_replicas: 1,
    reconciler_replicas: 1,
  });
});

test("every exact source instance blocks both source fleet counts", () => {
  const rows = [
    instance({ version: sourceVersion }),
    instance({
      id: "66666666-6666-4666-8666-666666666666",
      version: sourceVersion,
      state: "stopped",
    }),
    instance({
      id: "77777777-7777-4777-8777-777777777777",
      version: targetVersion,
    }),
  ];
  const attestation = observeBillingRolloutSourceFence(
    options(),
    fixtures({ scans: [rows, rows] }).inspect,
    clock,
  );
  assert.deepEqual(attestation.source_fleet, {
    api_replicas: 2,
    reconciler_replicas: 2,
  });
});

test("lifecycle gate presence and an immature absence fence remain nonzero", () => {
  const gatedWorker = workerVersion(["CP_PLAN_LIFECYCLE_ENABLED"]);
  const gated = observeBillingRolloutSourceFence(
    options(),
    fixtures({
      worker: gatedWorker,
      persistentSecrets: secrets(["CP_PLAN_LIFECYCLE_ENABLED"]),
    }).inspect,
    clock,
  );
  assert.equal(gated.fence.lifecycle_gate_present, true);
  assert.equal(gated.source_fleet.reconciler_replicas, 1);

  for (const lifecycleDisabledObservedAt of [
    null,
    "2026-08-17T22:06:01Z",
  ]) {
    const immature = observeBillingRolloutSourceFence(
      options({ lifecycleDisabledObservedAt }),
      fixtures().inspect,
      clock,
    );
    assert.equal(immature.source_fleet.api_replicas, 0);
    assert.equal(immature.source_fleet.reconciler_replicas, 1);
  }
  assert.equal(LIFECYCLE_IN_FLIGHT_BOUND_MS, 240_000);
});

test("billing cohort secret presence refuses an unknowable account count", () => {
  const cohortWorker = workerVersion(["CP_BILLING_ACCOUNT_ALLOWLIST"]);
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(),
      fixtures({
        worker: cohortWorker,
        persistentSecrets: secrets(["CP_BILLING_ACCOUNT_ALLOWLIST"]),
      }).inspect,
      clock,
    ),
    /zero-account cohort cannot be proven/,
  );

  const leaked = workerVersion();
  leaked.resources.bindings.push({
    name: "CP_BILLING_ACCOUNT_ALLOWLIST",
    type: "secret_text",
    text: "acct_secret_value",
  });
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(), fixtures({ worker: leaked }).inspect, clock,
    ),
    /invalid strict shape/,
  );
});

test("secret inventory disagreement and namespace ambiguity fail closed", () => {
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(),
      fixtures({ persistentSecrets: [] }).inspect,
      clock,
    ),
    /secret inventories disagreed/,
  );

  const wrongNamespace = application();
  wrongNamespace.durable_objects.namespace_id = "9".repeat(32);
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(), fixtures({ app: wrongNamespace }).inspect, clock,
    ),
    /did not match the active Backend namespace/,
  );
});

test("deployment, application, and instance drift each fail closed", () => {
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(),
      fixtures({
        deployments: [
          deployment(),
          deployment("88888888-8888-4888-8888-888888888888"),
        ],
      }).inspect,
      clock,
    ),
    /source state changed/,
  );

  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(),
      fixtures({
        applications: [application(), application(targetVersion + 1)],
      }).inspect,
      clock,
    ),
    /source state changed/,
  );

  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(),
      fixtures({
        scans: [
          [instance()],
          [instance({ state: "stopping" })],
        ],
      }).inspect,
      clock,
    ),
    /source state changed/,
  );
});

test("ambiguous instance rows and pagination fail closed", () => {
  const ambiguous = instance({ state: "running", version: null });
  assert.throws(
    () => observeBillingRolloutSourceFence(
      options(), fixtures({ scans: [[ambiguous], [ambiguous]] }).inspect, clock,
    ),
    /application version/,
  );

  const { inspect: baseInspect } = fixtures();
  let instanceCall = 0;
  const inspect = (args, operation) => {
    if (args[0] !== "containers" || args[1] !== "instances") {
      return baseInspect(args, operation);
    }
    instanceCall += 1;
    return instancePage([], null, "repeated-page-token");
  };
  assert.throws(
    () => observeBillingRolloutSourceFence(options(), inspect, clock),
    /continuation fence|pagination fence was invalid|pagination fence repeated/,
  );
});

test("instance inventory follows and fences every explicit JSON page", () => {
  const { inspect: baseInspect, calls } = fixtures({ scans: [[], []] });
  const first = instance();
  const second = instance({
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
    options(), inspect, clock,
  );
  assert.equal(attestation.fence.container_instance_count, 2);
  assert.equal(pageCall, 4);
  const continuedCalls = calls.filter((args) =>
    args[0] === "containers" && args[1] === "instances" &&
    args.includes("--page-token"));
  assert.equal(continuedCalls.length, 2);
  assert.equal(JSON.stringify(attestation).includes("next-private-page"), false);
});

test("strict CLI arguments require an explicit source application fence", () => {
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([]),
    /expected-source-application-id/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      "--expected-source-application-id", applicationID,
      "--expected-source-application-version", "17x",
    ]),
    /positive integer/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      "--expected-source-application-id", applicationID,
      "--expected-source-application-version", "017",
    ]),
    /canonical positive integer/,
  );
  assert.throws(
    () => parseBillingRolloutSourceFenceArgs([
      "--expected-source-application-id", applicationID,
      "--expected-source-application-id", applicationID,
      "--expected-source-application-version", String(sourceVersion),
    ]),
    /duplicate argument/,
  );
  const parsed = parseBillingRolloutSourceFenceArgs([
    "--expected-source-application-id", applicationID,
    "--expected-source-application-version", String(sourceVersion),
    "--lifecycle-disabled-observed-at", disabledAt,
    "--wrangler-cwd", "/private/wrangler",
    "--reviewed-env-file", "/private/wrangler.env",
  ]);
  assert.equal(parsed.expectedSourceApplicationID, applicationID);
  assert.equal(parsed.expectedSourceApplicationVersion, sourceVersion);
  assert.equal(parsed.lifecycleDisabledObservedAt, disabledAt);
});
