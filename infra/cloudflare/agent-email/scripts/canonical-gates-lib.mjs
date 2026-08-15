import { createHash } from "node:crypto";

import {
  validateAgentEmailOperationsLeaseEvidence,
} from "../../control-plane/src/agent-email-operations-lease.mjs";
import {
  parseManagedDeliveryAccountAllowlist,
} from "../../control-plane/src/agent-email-managed-delivery-cohort.mjs";

export const CANONICAL_GATES_WORKER = "witself-control-plane";
export const CANONICAL_GATES_OPERATION =
  "control_plane_canonical_gates_apply";
export const CANONICAL_GATES_PLAN_SCHEMA =
  "witself.agent-email-canonical-gates-plan.v1";
export const CANONICAL_GATES_RECEIPT_SCHEMA =
  "witself.agent-email-canonical-gates-receipt.v1";
export const CANONICAL_GATES_STATUS_SCHEMA =
  "witself.agent-email-canonical-gates-status.v1";
export const CANONICAL_GATE_NAMES = Object.freeze([
  "CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED",
  "CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED",
]);

const PLAN_LIFETIME_MS = 15 * 60 * 1_000;
const ACTIONS = new Set(["enable", "disable"]);
const ACCOUNT_ID = /^[0-9a-f]{32}(?![\s\S])/;
const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}(?![\s\S])/;
const SHA256 = /^[0-9a-f]{64}(?![\s\S])/;
const RELEASE_VERSION =
  /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?![\s\S])/;
const COMMIT = /^[0-9a-f]{40}(?![\s\S])/;
const RFC3339 =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})(?![\s\S])/;
const RFC3339_UTC =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z(?![\s\S])/;

function canonicalJSON(value) {
  const canonicalize = (item) => {
    if (Array.isArray(item)) return item.map(canonicalize);
    if (item && typeof item === "object") {
      return Object.fromEntries(
        Object.keys(item).sort().map((key) => [key, canonicalize(item[key])]),
      );
    }
    return item;
  };
  return JSON.stringify(canonicalize(value));
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function exactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      canonicalJSON(Object.keys(value).sort()) !==
        canonicalJSON([...keys].sort())) {
    throw new Error(`${label} was malformed`);
  }
}

function expectedChecks(action) {
  return [
    "target_identity_exact",
    "shared_durable_operations_lease",
    "active_control_plane_deployment_exact",
    "active_control_plane_release_exact",
    "all_binding_fingerprint_exact",
    "all_secret_name_fingerprint_exact",
    "founder_cohort_exact",
    "both_gates_transition_atomically",
    "unrelated_bindings_and_secrets_preserved",
    "exact_provider_readback",
    ...(action === "enable"
      ? ["ambiguous_enable_rolls_back_to_both_absent"]
      : ["ambiguous_disable_never_reenables"]),
  ];
}

function validateStateSummary(summary, label) {
  exactKeys(summary, [
    "worker_name", "deployment_id", "deployment_sha256", "version_id",
    "version_number", "release", "release_sha256", "bindings_count",
    "bindings_sha256", "secret_count", "secret_names_sha256",
    "founder_cohort", "gate_state", "gates",
  ], label);
  exactKeys(summary.release, ["version", "commit", "date"],
    `${label} release`);
  exactKeys(summary.founder_cohort, ["account_count", "allowlist_sha256"],
    `${label} Founder cohort`);
  exactKeys(summary.gates, CANONICAL_GATE_NAMES, `${label} gates`);
  for (const name of CANONICAL_GATE_NAMES) {
    exactKeys(summary.gates[name], ["bound", "listed"], `${label} gate`);
    if (typeof summary.gates[name].bound !== "boolean" ||
        typeof summary.gates[name].listed !== "boolean") {
      throw new Error(`${label} was malformed`);
    }
  }
  const gateEntries = Object.values(summary.gates);
  const gateState = gateEntries.every(({ bound, listed }) => !bound && !listed)
    ? "absent"
    : gateEntries.every(({ bound, listed }) => bound && listed)
      ? "present"
      : "mixed";
  if (summary.worker_name !== CANONICAL_GATES_WORKER ||
      !UUID.test(String(summary.deployment_id ?? "")) ||
      !UUID.test(String(summary.version_id ?? "")) ||
      !Number.isSafeInteger(summary.version_number) ||
      summary.version_number < 1 ||
      !RELEASE_VERSION.test(String(summary.release.version ?? "")) ||
      !COMMIT.test(String(summary.release.commit ?? "")) ||
      !RFC3339.test(String(summary.release.date ?? "")) ||
      !Number.isFinite(Date.parse(summary.release.date)) ||
      sha256(canonicalJSON(summary.release)) !== summary.release_sha256 ||
      !Number.isSafeInteger(summary.bindings_count) ||
      summary.bindings_count < 1 || summary.bindings_count > 10_000 ||
      !Number.isSafeInteger(summary.secret_count) ||
      summary.secret_count < 0 || summary.secret_count > 10_000 ||
      !Number.isSafeInteger(summary.founder_cohort.account_count) ||
      summary.founder_cohort.account_count < 0 ||
      summary.founder_cohort.account_count > 100 ||
      gateState !== summary.gate_state ||
      ![
        summary.deployment_sha256,
        summary.release_sha256,
        summary.bindings_sha256,
        summary.secret_names_sha256,
        summary.founder_cohort.allowlist_sha256,
      ].every((value) => SHA256.test(String(value ?? "")))) {
    throw new Error(`${label} was malformed`);
  }
  return summary;
}

function activeVersion(deployment) {
  if (!deployment || typeof deployment !== "object" ||
      Array.isArray(deployment) || !UUID.test(String(deployment.id ?? "")) ||
      deployment.strategy !== "percentage" ||
      !Array.isArray(deployment.versions) || deployment.versions.length !== 1 ||
      deployment.versions[0]?.percentage !== 100 ||
      !UUID.test(String(deployment.versions[0]?.version_id ?? ""))) {
    throw new Error("control-plane deployment was not one version at 100 percent");
  }
  return deployment.versions[0].version_id;
}

function normalizeBindings(version, expectedVersionID) {
  if (!version || typeof version !== "object" || Array.isArray(version) ||
      version.id !== expectedVersionID ||
      !UUID.test(String(version.id ?? "")) ||
      !Number.isSafeInteger(version.number) || version.number < 1 ||
      !Array.isArray(version.resources?.bindings)) {
    throw new Error("control-plane active Worker version was invalid");
  }
  const names = new Set();
  const bindings = version.resources.bindings.map((binding) => {
    if (!binding || typeof binding !== "object" || Array.isArray(binding) ||
        typeof binding.name !== "string" || binding.name.length < 1 ||
        binding.name.length > 255 || binding.name !== binding.name.trim() ||
        typeof binding.type !== "string" || binding.type.length < 1 ||
        names.has(binding.name)) {
      throw new Error("control-plane active Worker bindings were invalid");
    }
    names.add(binding.name);
    return structuredClone(binding);
  }).sort((left, right) => left.name.localeCompare(right.name));
  return Object.freeze(bindings);
}

function plainBinding(bindings, name) {
  const binding = bindings.find((candidate) => candidate.name === name);
  if (binding?.type !== "plain_text" || typeof binding.text !== "string") {
    throw new Error(`control-plane active Worker was missing ${name}`);
  }
  return binding.text;
}

function normalizeRelease(bindings) {
  const release = Object.freeze({
    version: plainBinding(bindings, "WITSELF_EDGE_RELEASE_VERSION"),
    commit: plainBinding(bindings, "WITSELF_EDGE_RELEASE_COMMIT"),
    date: plainBinding(bindings, "WITSELF_EDGE_RELEASE_DATE"),
  });
  if (!RELEASE_VERSION.test(release.version) ||
      !COMMIT.test(release.commit) || !RFC3339.test(release.date) ||
      !Number.isFinite(Date.parse(release.date))) {
    throw new Error("control-plane active Worker release identity was invalid");
  }
  return release;
}

function normalizeSecretInventory(raw) {
  if (!Array.isArray(raw)) {
    throw new Error("control-plane Worker secret inventory was invalid");
  }
  const names = new Set();
  return Object.freeze(raw.map((secret) => {
    if (!secret || typeof secret !== "object" || Array.isArray(secret) ||
        typeof secret.name !== "string" || secret.name.length < 1 ||
        secret.name.length > 255 || secret.name !== secret.name.trim() ||
        typeof secret.type !== "string" || secret.type.length < 1 ||
        names.has(secret.name) || Object.hasOwn(secret, "text") ||
        Object.hasOwn(secret, "value")) {
      throw new Error("control-plane Worker secret inventory was invalid");
    }
    names.add(secret.name);
    return Object.freeze({ name: secret.name, type: secret.type });
  }).sort((left, right) => left.name.localeCompare(right.name)));
}

function gateSummary(bindings, secrets) {
  const gates = {};
  for (const name of CANONICAL_GATE_NAMES) {
    const binding = bindings.find((candidate) => candidate.name === name);
    const secret = secrets.find((candidate) => candidate.name === name);
    const bound = binding?.type === "secret_text" &&
      !Object.hasOwn(binding, "text");
    const listed = secret?.type === "secret_text";
    gates[name] = Object.freeze({ bound, listed });
  }
  const entries = Object.values(gates);
  const state = entries.every(({ bound, listed }) => !bound && !listed)
    ? "absent"
    : entries.every(({ bound, listed }) => bound && listed)
      ? "present"
      : "mixed";
  return Object.freeze({ state, gates: Object.freeze(gates) });
}

function summarizeWorker(raw, secrets) {
  const deployment = raw?.control_plane_deployment;
  const version = raw?.control_plane_version;
  const versionID = activeVersion(deployment);
  const bindings = normalizeBindings(version, versionID);
  const release = normalizeRelease(bindings);
  const cohortRaw = plainBinding(
    bindings,
    "CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST",
  );
  let cohort;
  try {
    cohort = parseManagedDeliveryAccountAllowlist(cohortRaw);
  } catch {
    throw new Error("control-plane Founder delivery cohort was invalid");
  }
  const gate = gateSummary(bindings, secrets);
  const secretNames = secrets.map(({ name }) => name);
  return Object.freeze({
    private: Object.freeze({ bindings, secrets }),
    summary: Object.freeze({
      worker_name: CANONICAL_GATES_WORKER,
      deployment_id: deployment.id,
      deployment_sha256: sha256(canonicalJSON(deployment)),
      version_id: versionID,
      version_number: version.number,
      release,
      release_sha256: sha256(canonicalJSON(release)),
      bindings_count: bindings.length,
      bindings_sha256: sha256(canonicalJSON(bindings)),
      secret_count: secrets.length,
      secret_names_sha256: sha256(canonicalJSON(secretNames)),
      founder_cohort: Object.freeze({
        account_count: cohort.length,
        allowlist_sha256: sha256(cohortRaw),
      }),
      gate_state: gate.state,
      gates: gate.gates,
    }),
  });
}

export async function captureCanonicalGatesState(api, runtime) {
  if (!api || !ACCOUNT_ID.test(String(api.accountID ?? "")) ||
      typeof api.listControlPlaneSecrets !== "function" ||
      !runtime || (typeof runtime.inspectControlPlane !== "function" &&
        typeof runtime.inspectWorkers !== "function")) {
    throw new Error("canonical gates inspection runtime was invalid");
  }
  const inspectControlPlane = typeof runtime.inspectControlPlane === "function"
    ? () => runtime.inspectControlPlane()
    : () => runtime.inspectWorkers();
  const [workers, rawSecrets] = await Promise.all([
    inspectControlPlane(),
    api.listControlPlaneSecrets(),
  ]);
  return summarizeWorker(workers, normalizeSecretInventory(rawSecrets));
}

function actionReady(action, summary) {
  if (summary.gate_state === "mixed") return false;
  if (action === "enable") {
    return summary.gate_state === "absent" &&
      summary.founder_cohort.account_count === 1;
  }
  return summary.gate_state === "present";
}

function assertActionPreconditions(action, summary) {
  if (!actionReady(action, summary)) {
    throw new Error(
      `control-plane canonical gates ${action} precondition was not ready`,
    );
  }
}

function planBody(api, action, state, createdAt) {
  return {
    schema: CANONICAL_GATES_PLAN_SCHEMA,
    action,
    created_at: createdAt.toISOString(),
    expires_at: new Date(
      createdAt.valueOf() + PLAN_LIFETIME_MS,
    ).toISOString(),
    target: {
      account_id: api.accountID,
      worker_name: CANONICAL_GATES_WORKER,
    },
    precondition: state.summary,
    desired_gate_state: action === "enable" ? "present" : "absent",
    checks: expectedChecks(action),
  };
}

function withFence(body) {
  return Object.freeze({
    ...body,
    apply_fence: Object.freeze({
      algorithm: "sha256",
      sha256: sha256(canonicalJSON(body)),
    }),
  });
}

export async function inspectCanonicalGates(api, runtime) {
  const state = await captureCanonicalGatesState(api, runtime);
  return Object.freeze({
    schema: CANONICAL_GATES_STATUS_SCHEMA,
    control_plane: state.summary,
    ready_to_enable: actionReady("enable", state.summary),
    ready_to_disable: actionReady("disable", state.summary),
  });
}

export async function createCanonicalGatesPlan(
  api,
  runtime,
  action,
  { now = () => new Date(), createdAt = null } = {},
) {
  if (!ACTIONS.has(action)) {
    throw new Error("canonical gates action was invalid");
  }
  const clock = now();
  if (!(clock instanceof Date) || !Number.isFinite(clock.valueOf())) {
    throw new Error("canonical gates planner clock was invalid");
  }
  const creation = createdAt === null ? clock : new Date(createdAt);
  if (!Number.isFinite(creation.valueOf())) {
    throw new Error("canonical gates plan creation time was invalid");
  }
  const state = await captureCanonicalGatesState(api, runtime);
  assertActionPreconditions(action, state.summary);
  return withFence(planBody(api, action, state, creation));
}

export function verifyCanonicalGatesPlan(
  plan,
  suppliedSHA256,
  { now = () => new Date() } = {},
) {
  exactKeys(plan, [
    "schema", "action", "created_at", "expires_at", "target",
    "precondition", "desired_gate_state", "checks", "apply_fence",
  ], "canonical gates plan");
  exactKeys(plan.target, ["account_id", "worker_name"],
    "canonical gates target");
  exactKeys(plan.apply_fence, ["algorithm", "sha256"],
    "canonical gates apply fence");
  validateStateSummary(plan.precondition, "canonical gates precondition");
  const clock = now();
  if (plan.schema !== CANONICAL_GATES_PLAN_SCHEMA ||
      !ACTIONS.has(plan.action) ||
      !RFC3339_UTC.test(String(plan.created_at ?? "")) ||
      !RFC3339_UTC.test(String(plan.expires_at ?? "")) ||
      Date.parse(plan.expires_at) !==
        Date.parse(plan.created_at) + PLAN_LIFETIME_MS ||
      !(clock instanceof Date) || !Number.isFinite(clock.valueOf()) ||
      Date.parse(plan.created_at) > clock.valueOf() + 300_000 ||
      clock.valueOf() > Date.parse(plan.expires_at) ||
      !ACCOUNT_ID.test(String(plan.target.account_id ?? "")) ||
      plan.target.worker_name !== CANONICAL_GATES_WORKER ||
      plan.desired_gate_state !==
        (plan.action === "enable" ? "present" : "absent") ||
      canonicalJSON(plan.checks) !== canonicalJSON(expectedChecks(plan.action)) ||
      plan.precondition.gate_state !==
        (plan.action === "enable" ? "absent" : "present") ||
      (plan.action === "enable" &&
        plan.precondition.founder_cohort.account_count !== 1) ||
      plan.apply_fence.algorithm !== "sha256" ||
      !SHA256.test(String(plan.apply_fence.sha256 ?? ""))) {
    throw new Error("canonical gates plan was malformed or expired");
  }
  const { apply_fence: ignored, ...body } = plan;
  const calculated = sha256(canonicalJSON(body));
  if (calculated !== plan.apply_fence.sha256) {
    throw new Error("canonical gates plan fence did not match its content");
  }
  if (!SHA256.test(String(suppliedSHA256 ?? "")) ||
      suppliedSHA256 !== calculated) {
    throw new Error("--plan-sha256 did not match the exact canonical gates plan");
  }
  return calculated;
}

function exactPlan(actual, expected) {
  if (canonicalJSON(actual) !== canonicalJSON(expected)) {
    throw new Error(
      "canonical gates plan preconditions changed; create and review a new plan",
    );
  }
}

function withoutTargetBindings(bindings) {
  return bindings.filter(({ name }) => !CANONICAL_GATE_NAMES.includes(name));
}

function withoutTargetSecrets(secrets) {
  return secrets.filter(({ name }) => !CANONICAL_GATE_NAMES.includes(name));
}

function assertUnrelatedPreserved(before, after) {
  if (canonicalJSON(withoutTargetBindings(before.private.bindings)) !==
        canonicalJSON(withoutTargetBindings(after.private.bindings)) ||
      canonicalJSON(withoutTargetSecrets(before.private.secrets)) !==
        canonicalJSON(withoutTargetSecrets(after.private.secrets)) ||
      before.summary.release_sha256 !== after.summary.release_sha256 ||
      canonicalJSON(before.summary.founder_cohort) !==
        canonicalJSON(after.summary.founder_cohort)) {
    throw new Error("canonical gates unrelated Worker state changed");
  }
}

function assertPostcondition(plan, before, after) {
  assertUnrelatedPreserved(before, after);
  if (after.summary.gate_state !== plan.desired_gate_state ||
      after.summary.deployment_id === before.summary.deployment_id ||
      after.summary.version_id === before.summary.version_id) {
    throw new Error("canonical gates provider readback was not exact");
  }
}

function mutationBody(action) {
  return Object.freeze(Object.fromEntries(CANONICAL_GATE_NAMES.map((name) => [
    name,
    action === "enable"
      ? Object.freeze({ name, text: "true", type: "secret_text" })
      : null,
  ])));
}

async function patchGates(api, action, leaseGuard) {
  await leaseGuard.renew();
  await api.patchControlPlaneSecrets(mutationBody(action));
  await leaseGuard.renew();
}

async function failClosedEnable(api, runtime, before, leaseGuard) {
  await patchGates(api, "disable", leaseGuard);
  const restored = await captureCanonicalGatesState(api, runtime);
  assertUnrelatedPreserved(before, restored);
  if (restored.summary.gate_state !== "absent") {
    throw new Error("canonical gates fail-closed rollback was incomplete");
  }
}

function receiptBody(plan, fence, before, after, leaseEvidence) {
  return {
    schema: CANONICAL_GATES_RECEIPT_SCHEMA,
    outcome: "verified",
    action: plan.action,
    plan_sha256: fence,
    target: plan.target,
    operations_lease: leaseEvidence,
    before: before.summary,
    after: after.summary,
  };
}

function withReceiptFence(body) {
  return Object.freeze({
    ...body,
    receipt_fence: Object.freeze({
      algorithm: "sha256",
      sha256: sha256(canonicalJSON(body)),
    }),
  });
}

export function verifyCanonicalGatesReceipt(receipt) {
  exactKeys(receipt, [
    "schema", "outcome", "action", "plan_sha256", "target",
    "operations_lease", "before", "after", "receipt_fence",
  ], "canonical gates receipt");
  exactKeys(receipt.target, ["account_id", "worker_name"],
    "canonical gates receipt target");
  exactKeys(receipt.receipt_fence, ["algorithm", "sha256"],
    "canonical gates receipt fence");
  validateStateSummary(receipt.before, "canonical gates receipt before");
  validateStateSummary(receipt.after, "canonical gates receipt after");
  const countDirection = receipt.action === "enable" ? 2 : -2;
  if (receipt.schema !== CANONICAL_GATES_RECEIPT_SCHEMA ||
      receipt.outcome !== "verified" || !ACTIONS.has(receipt.action) ||
      !SHA256.test(String(receipt.plan_sha256 ?? "")) ||
      !ACCOUNT_ID.test(String(receipt.target.account_id ?? "")) ||
      receipt.target.worker_name !== CANONICAL_GATES_WORKER ||
      receipt.before?.gate_state !==
        (receipt.action === "enable" ? "absent" : "present") ||
      receipt.after?.gate_state !==
        (receipt.action === "enable" ? "present" : "absent") ||
      receipt.after.bindings_count !==
        receipt.before.bindings_count + countDirection ||
      receipt.after.secret_count !==
        receipt.before.secret_count + countDirection ||
      receipt.after.release_sha256 !== receipt.before.release_sha256 ||
      canonicalJSON(receipt.after.founder_cohort) !==
        canonicalJSON(receipt.before.founder_cohort) ||
      receipt.after.deployment_id === receipt.before.deployment_id ||
      receipt.after.version_id === receipt.before.version_id ||
      receipt.receipt_fence.algorithm !== "sha256" ||
      !SHA256.test(String(receipt.receipt_fence.sha256 ?? ""))) {
    throw new Error("canonical gates receipt was malformed");
  }
  validateAgentEmailOperationsLeaseEvidence(
    receipt.operations_lease,
    CANONICAL_GATES_OPERATION,
  );
  const { receipt_fence: ignored, ...body } = receipt;
  if (sha256(canonicalJSON(body)) !== receipt.receipt_fence.sha256) {
    throw new Error("canonical gates receipt fence was invalid");
  }
  return structuredClone(receipt);
}

export async function applyCanonicalGatesPlan(
  plan,
  suppliedSHA256,
  api,
  runtime,
  { now = () => new Date() } = {},
) {
  const fence = verifyCanonicalGatesPlan(plan, suppliedSHA256, { now });
  if (!runtime?.operationsLease ||
      typeof runtime.operationsLease.run !== "function" ||
      typeof api?.patchControlPlaneSecrets !== "function") {
    throw new Error("canonical gates mutation runtime was invalid");
  }
  return runtime.operationsLease.run(
    CANONICAL_GATES_OPERATION,
    async (leaseGuard) => {
      const reviewed = await createCanonicalGatesPlan(
        api,
        runtime,
        plan.action,
        { now, createdAt: plan.created_at },
      );
      exactPlan(reviewed, plan);
      const before = await captureCanonicalGatesState(api, runtime);
      if (canonicalJSON(before.summary) !==
          canonicalJSON(plan.precondition)) {
        throw new Error(
          "canonical gates state changed immediately before mutation",
        );
      }

      let mutationError;
      let verificationError;
      let recoveryError;
      let after;
      await leaseGuard.renew();
      verifyCanonicalGatesPlan(plan, suppliedSHA256, { now });
      try {
        await api.patchControlPlaneSecrets(mutationBody(plan.action));
        await leaseGuard.renew();
      } catch (error) {
        mutationError = error;
      }
      if (!mutationError) {
        try {
          after = await captureCanonicalGatesState(api, runtime);
          assertPostcondition(plan, before, after);
        } catch (error) {
          verificationError = error;
        }
      }
      if ((mutationError || verificationError) && plan.action === "enable") {
        try {
          await failClosedEnable(api, runtime, before, leaseGuard);
        } catch (error) {
          recoveryError = error;
        }
      }
      const errors = [mutationError, verificationError, recoveryError]
        .filter(Boolean);
      if (errors.length > 1) {
        throw new AggregateError(
          errors,
          "canonical gates mutation failed and rollback was incomplete",
        );
      }
      if (errors.length === 1) throw errors[0];

      const leaseEvidence = leaseGuard.evidence();
      validateAgentEmailOperationsLeaseEvidence(
        leaseEvidence,
        CANONICAL_GATES_OPERATION,
      );
      return withReceiptFence(receiptBody(
        plan,
        fence,
        before,
        after,
        leaseEvidence,
      ));
    },
  );
}

export const canonicalGatesInternals = Object.freeze({
  canonicalJSON,
  mutationBody,
  normalizeSecretInventory,
  sha256,
});
