import {
  canonicalJSON,
  capturePrimaryRoutingState,
  inspectPrimaryReadiness,
  normalizePrimaryCanaryManifest,
  PRIMARY_CANARY_DOMAIN,
  PRIMARY_CANARY_WORKER,
  sha256,
  summarizePrimaryReadiness,
} from "./primary-routing-lib.mjs";
import {
  validateAgentEmailOperationsLeaseEvidence,
} from "../../control-plane/src/agent-email-operations-lease.mjs";

export const CATCH_ALL_PLAN_SCHEMA = "witself.agent-email-catch-all-plan.v2";
export const CATCH_ALL_RECEIPT_SCHEMA = "witself.agent-email-catch-all-receipt.v2";
export const CATCH_ALL_STATUS_SCHEMA = "witself.agent-email-catch-all-status.v1";

const PLAN_LIFETIME_MS = 15 * 60 * 1_000;
const ACTIONS = new Set(["enable", "disable", "rollback"]);
const ID = /^[0-9a-f]{32}(?![\s\S])/;
const SHA256 = /^[0-9a-f]{64}(?![\s\S])/;
const RFC3339_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z(?![\s\S])/;
const REVIEW_ID = /^[A-Za-z0-9][A-Za-z0-9._:/-]{2,127}(?![\s\S])/;
const WORKER_RULE_NAME = "Witself agent email catch-all";

function exactKeys(value, keys, label) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
      canonicalJSON(Object.keys(value).sort()) !== canonicalJSON([...keys].sort())) {
    throw new Error(`${label} was malformed`);
  }
}

function validEmail(value) {
  return typeof value === "string" && value === value.trim() && value.length <= 320 &&
    /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

export function normalizeCatchAllContract(rule) {
  if (!rule || typeof rule !== "object" || Array.isArray(rule) ||
      typeof rule.name !== "string" || rule.name !== rule.name.trim() ||
      rule.name.length < 1 || rule.name.length > 256 ||
      typeof rule.enabled !== "boolean" ||
      !Array.isArray(rule.matchers) || rule.matchers.length !== 1 ||
      rule.matchers[0]?.type !== "all" ||
      !Array.isArray(rule.actions) || rule.actions.length !== 1) {
    throw new Error("Cloudflare catch-all writable contract was invalid");
  }
  const action = rule.actions[0];
  if (!action || typeof action !== "object" || Array.isArray(action) ||
      !["drop", "forward", "worker"].includes(action.type)) {
    throw new Error("Cloudflare catch-all writable contract was invalid");
  }
  const values = action.value ?? [];
  if (!Array.isArray(values) ||
      (action.type === "drop" && values.length !== 0) ||
      (action.type === "forward" &&
        (values.length < 1 || values.some((value) => !validEmail(value)))) ||
      (action.type === "worker" &&
        (values.length !== 1 || values[0] !== PRIMARY_CANARY_WORKER))) {
    throw new Error("Cloudflare catch-all writable contract was invalid");
  }
  return Object.freeze({
    name: rule.name,
    enabled: rule.enabled,
    matchers: [{ type: "all" }],
    actions: [{ type: action.type, value: [...values] }],
  });
}

function enabledWorkerContract() {
  return Object.freeze({
    name: WORKER_RULE_NAME,
    enabled: true,
    matchers: [{ type: "all" }],
    actions: [{ type: "worker", value: [PRIMARY_CANARY_WORKER] }],
  });
}

function normalizeReview(review) {
  exactKeys(
    review,
    ["change_id", "provider_contract_review_sha256"],
    "catch-all external review",
  );
  if (!REVIEW_ID.test(String(review.change_id ?? "")) ||
      !SHA256.test(String(review.provider_contract_review_sha256 ?? ""))) {
    throw new Error("catch-all external review was invalid");
  }
  return Object.freeze({
    change_id: review.change_id,
    provider_contract_review_sha256: review.provider_contract_review_sha256,
  });
}

function catchAllSnapshot(routing) {
  if (routing.catchAll?.source !== "api") {
    throw new Error("Cloudflare catch-all is not API-owned and cannot be mutated");
  }
  const contract = normalizeCatchAllContract(routing.catchAll);
  return Object.freeze({
    contract,
    contract_sha256: sha256(canonicalJSON(contract)),
    provider_sha256: routing.summary.catch_all.sha256,
    role_routes_sha256: routing.summary.role_routes.sha256,
    role_routes_ready: routing.summary.role_routes.ready,
    managed_rules_sha256: routing.summary.managed_rules.sha256,
    managed_rules: routing.summary.managed_rules,
    email_routing: routing.summary.email_routing,
  });
}

function canaryIsActive(snapshot, expectedCount) {
  return snapshot.managed_rules.configured === expectedCount &&
    snapshot.managed_rules.enabled === expectedCount &&
    snapshot.managed_rules.disabled === 0 &&
    snapshot.managed_rules.missing === 0 &&
    snapshot.managed_rules.stale === 0 &&
    snapshot.managed_rules.conflicts === 0;
}

function assertEnablePreconditions(snapshot, readiness, manifest) {
  if (snapshot.contract.enabled ||
      snapshot.email_routing.enabled !== true || snapshot.email_routing.ready !== true ||
      snapshot.email_routing.support_subaddress !== true ||
      snapshot.role_routes_ready !== true ||
      !canaryIsActive(snapshot, manifest.agents.length) ||
      readiness?.activation_ready !== true) {
    throw new Error("catch-all enable foundation was not ready");
  }
}

function disabledContract(contract) {
  return Object.freeze({ ...contract, enabled: false });
}

function planBody(api, action, manifest, snapshot, desired, review, readiness, createdAt, rollbackOf = "") {
  return {
    schema: CATCH_ALL_PLAN_SCHEMA,
    action,
    domain: PRIMARY_CANARY_DOMAIN,
    worker: PRIMARY_CANARY_WORKER,
    created_at: createdAt.toISOString(),
    expires_at: new Date(createdAt.valueOf() + PLAN_LIFETIME_MS).toISOString(),
    target: {
      account_id: api.accountID,
      zone_id: api.zoneID,
      route_directory_id: api.namespaceID,
    },
    manifest,
    ...(review ? { external_review: review } : {}),
    ...(rollbackOf ? { rollback_of_receipt_sha256: rollbackOf } : {}),
    precondition: {
      catch_all: snapshot,
      ...(readiness ? { readiness } : {}),
    },
    desired_catch_all: desired,
    checks: [
      "target_ids_exact",
      "shared_durable_operations_lease",
      "catch_all_provider_fingerprint_exact",
      "operator_role_fingerprint_preserved",
      "primary_canary_fingerprint_preserved",
      "post_mutation_readback_exact",
      ...(action === "enable"
        ? [
          "external_provider_contract_review_fenced",
          "primary_canary_active",
          "signed_canonical_projection_ready",
          "canonical_delivery_gates_ready",
          "managed_alias_delivery_disabled",
        ]
        : []),
      ...(action === "rollback" ? ["rollback_target_remains_disabled"] : []),
    ],
  };
}

function withFence(body) {
  return Object.freeze({
    ...body,
    apply_fence: {
      algorithm: "sha256",
      sha256: sha256(canonicalJSON(body)),
    },
  });
}

export async function createCatchAllPlan(api, runtime, manifestInput, action, {
  review = null,
  now = () => new Date(),
  createdAt = null,
  rollbackReceipt = null,
} = {}) {
  if (!ACTIONS.has(action)) throw new Error("catch-all action was invalid");
  const clock = now();
  if (!(clock instanceof Date) || !Number.isFinite(clock.valueOf())) {
    throw new Error("catch-all planner clock was invalid");
  }
  const creation = createdAt === null ? clock : new Date(createdAt);
  if (!Number.isFinite(creation.valueOf())) throw new Error("catch-all plan time was invalid");
  const manifest = normalizePrimaryCanaryManifest(manifestInput);
  const routing = await capturePrimaryRoutingState(api, manifest);
  const snapshot = catchAllSnapshot(routing);

  let readiness = null;
  let desired;
  let normalizedReview = null;
  let rollbackOf = "";
  if (action === "enable") {
    normalizedReview = normalizeReview(review);
    readiness = await inspectPrimaryReadiness(api, runtime, manifest, { now: () => clock });
    readiness = Object.freeze({ ...readiness, checked_at: creation.toISOString() });
    assertEnablePreconditions(snapshot, readiness, manifest);
    desired = enabledWorkerContract();
  } else if (action === "disable") {
    if (!snapshot.contract.enabled) throw new Error("catch-all is already disabled");
    desired = disabledContract(snapshot.contract);
  } else {
    const receipt = verifyCatchAllReceipt(rollbackReceipt);
    if (receipt.action !== "enable" || receipt.before_contract.enabled !== false ||
        receipt.after_contract.enabled !== true ||
        receipt.target.account_id !== api.accountID || receipt.target.zone_id !== api.zoneID ||
        receipt.target.route_directory_id !== api.namespaceID ||
        canonicalJSON(snapshot.contract) !== canonicalJSON(receipt.after_contract) ||
        snapshot.role_routes_sha256 !== receipt.guards.role_routes_sha256 ||
        snapshot.managed_rules_sha256 !== receipt.guards.managed_rules_sha256) {
      throw new Error("catch-all rollback receipt did not match current state");
    }
    desired = receipt.before_contract;
    rollbackOf = receipt.receipt_fence.sha256;
  }
  return withFence(planBody(
    api,
    action,
    manifest,
    snapshot,
    desired,
    normalizedReview,
    readiness ? summarizePrimaryReadiness(readiness) : null,
    creation,
    rollbackOf,
  ));
}

export function verifyCatchAllPlan(plan, suppliedSHA256, {
  now = () => new Date(),
} = {}) {
  const expectedKeys = [
    "schema", "action", "domain", "worker", "created_at", "expires_at",
    "target", "manifest", "precondition", "desired_catch_all", "checks",
    "apply_fence",
    ...(plan?.action === "enable" ? ["external_review"] : []),
    ...(plan?.action === "rollback" ? ["rollback_of_receipt_sha256"] : []),
  ];
  exactKeys(plan, expectedKeys, "catch-all plan");
  exactKeys(plan.target, ["account_id", "zone_id", "route_directory_id"], "catch-all target");
  exactKeys(plan.apply_fence, ["algorithm", "sha256"], "catch-all apply fence");
  const manifest = normalizePrimaryCanaryManifest(plan.manifest);
  const desired = normalizeCatchAllContract(plan.desired_catch_all);
  const clock = now();
  if (plan.schema !== CATCH_ALL_PLAN_SCHEMA || !ACTIONS.has(plan.action) ||
      plan.domain !== PRIMARY_CANARY_DOMAIN || plan.worker !== PRIMARY_CANARY_WORKER ||
      manifest.domain !== plan.domain || manifest.worker_name !== plan.worker ||
      !RFC3339_UTC.test(plan.created_at) || !RFC3339_UTC.test(plan.expires_at) ||
      Date.parse(plan.expires_at) !== Date.parse(plan.created_at) + PLAN_LIFETIME_MS ||
      !(clock instanceof Date) || !Number.isFinite(clock.valueOf()) ||
      Date.parse(plan.created_at) > clock.valueOf() + 300_000 ||
      clock.valueOf() > Date.parse(plan.expires_at) ||
      !ID.test(plan.target.account_id) || !ID.test(plan.target.zone_id) ||
      !ID.test(plan.target.route_directory_id) || !Array.isArray(plan.checks) ||
      plan.checks.length < 5 || plan.apply_fence.algorithm !== "sha256" ||
      !SHA256.test(plan.apply_fence.sha256) ||
      canonicalJSON(desired) !== canonicalJSON(plan.desired_catch_all) ||
      (plan.action === "enable" &&
        (desired.enabled !== true || !normalizeReview(plan.external_review))) ||
      (plan.action !== "enable" && desired.enabled !== false) ||
      (plan.action === "rollback" && !SHA256.test(plan.rollback_of_receipt_sha256))) {
    throw new Error("catch-all plan was malformed or expired");
  }
  const { apply_fence: ignored, ...body } = plan;
  const calculated = sha256(canonicalJSON(body));
  if (calculated !== plan.apply_fence.sha256) {
    throw new Error("catch-all plan fence did not match its content");
  }
  if (!SHA256.test(String(suppliedSHA256 ?? "")) || calculated !== suppliedSHA256) {
    throw new Error("--plan-sha256 did not match the exact catch-all plan");
  }
  return calculated;
}

function exactPlan(actual, expected) {
  if (canonicalJSON(actual) !== canonicalJSON(expected)) {
    throw new Error("catch-all plan preconditions changed; create and review a new plan");
  }
}

function receiptBody(plan, fence, before, after, operationsLease) {
  return {
    schema: CATCH_ALL_RECEIPT_SCHEMA,
    outcome: "verified",
    action: plan.action,
    domain: plan.domain,
    worker: plan.worker,
    plan_sha256: fence,
    target: plan.target,
    operations_lease: operationsLease,
    before_contract: before.contract,
    after_contract: after.contract,
    guards: {
      role_routes_sha256: after.role_routes_sha256,
      managed_rules_sha256: after.managed_rules_sha256,
    },
  };
}

function withReceiptFence(body) {
  return Object.freeze({
    ...body,
    receipt_fence: {
      algorithm: "sha256",
      sha256: sha256(canonicalJSON(body)),
    },
  });
}

export function verifyCatchAllReceipt(receipt) {
  exactKeys(receipt, [
    "schema", "outcome", "action", "domain", "worker", "plan_sha256",
    "target", "operations_lease", "before_contract", "after_contract",
    "guards", "receipt_fence",
  ], "catch-all receipt");
  exactKeys(receipt.target, ["account_id", "zone_id", "route_directory_id"], "catch-all receipt target");
  exactKeys(receipt.guards, ["role_routes_sha256", "managed_rules_sha256"], "catch-all receipt guards");
  exactKeys(receipt.receipt_fence, ["algorithm", "sha256"], "catch-all receipt fence");
  validateAgentEmailOperationsLeaseEvidence(
    receipt.operations_lease,
    "catch_all_routing_apply",
  );
  const before = normalizeCatchAllContract(receipt.before_contract);
  const after = normalizeCatchAllContract(receipt.after_contract);
  const { receipt_fence: ignored, ...body } = receipt;
  const calculated = sha256(canonicalJSON(body));
  if (receipt.schema !== CATCH_ALL_RECEIPT_SCHEMA || receipt.outcome !== "verified" ||
      !ACTIONS.has(receipt.action) || receipt.domain !== PRIMARY_CANARY_DOMAIN ||
      receipt.worker !== PRIMARY_CANARY_WORKER || !SHA256.test(receipt.plan_sha256) ||
      canonicalJSON(before) !== canonicalJSON(receipt.before_contract) ||
      canonicalJSON(after) !== canonicalJSON(receipt.after_contract) ||
      !ID.test(receipt.target.account_id) || !ID.test(receipt.target.zone_id) ||
      !ID.test(receipt.target.route_directory_id) ||
      !SHA256.test(receipt.guards.role_routes_sha256) ||
      !SHA256.test(receipt.guards.managed_rules_sha256) ||
      receipt.receipt_fence.algorithm !== "sha256" ||
      !SHA256.test(receipt.receipt_fence.sha256) ||
      receipt.receipt_fence.sha256 !== calculated) {
    throw new Error("catch-all receipt was malformed");
  }
  return receipt;
}

async function reconstructPlan(plan, api, runtime, now) {
  if (plan.action === "rollback") {
    const clock = now();
    const manifest = normalizePrimaryCanaryManifest(plan.manifest);
    const routing = await capturePrimaryRoutingState(api, manifest);
    const snapshot = catchAllSnapshot(routing);
    if (snapshot.contract.enabled !== true || plan.desired_catch_all.enabled !== false) {
      throw new Error("catch-all rollback no longer had an enabled source and disabled target");
    }
    return withFence(planBody(
      api,
      "rollback",
      manifest,
      snapshot,
      normalizeCatchAllContract(plan.desired_catch_all),
      null,
      null,
      new Date(plan.created_at),
      plan.rollback_of_receipt_sha256,
    ));
  }
  return createCatchAllPlan(api, runtime, plan.manifest, plan.action, {
    review: plan.external_review ?? null,
    now,
    createdAt: plan.created_at,
  });
}

export async function applyCatchAllPlan(plan, suppliedSHA256, api, runtime, {
  now = () => new Date(),
} = {}) {
  const fence = verifyCatchAllPlan(plan, suppliedSHA256, { now });
  if (!api || typeof api.replaceCatchAll !== "function") {
    throw new Error("catch-all apply runtime had no mutation capability");
  }
  if (api.accountID !== plan.target.account_id || api.zoneID !== plan.target.zone_id ||
      api.namespaceID !== plan.target.route_directory_id) {
    throw new Error("catch-all plan targeted another Cloudflare resource");
  }
  if (!runtime?.operationsLease ||
      typeof runtime.operationsLease.run !== "function") {
    throw new Error("catch-all durable operations lease was unavailable");
  }
  return runtime.operationsLease.run(
    "catch_all_routing_apply",
    async (leaseGuard) => {
      const reviewed = await reconstructPlan(plan, api, runtime, now);
      exactPlan(reviewed, plan);
      const manifest = normalizePrimaryCanaryManifest(plan.manifest);
      const beforeRouting = await capturePrimaryRoutingState(api, manifest);
      const before = catchAllSnapshot(beforeRouting);
      if (canonicalJSON(before) !== canonicalJSON(plan.precondition.catch_all)) {
        throw new Error("catch-all state changed immediately before mutation; create and review a new plan");
      }
      let mutationError;
      try {
        await leaseGuard.renew();
        await api.replaceCatchAll(plan.desired_catch_all);
        await leaseGuard.renew();
      } catch (error) {
        mutationError = error;
      }
      let after;
      let verificationError;
      try {
        after = catchAllSnapshot(await capturePrimaryRoutingState(api, manifest));
        await leaseGuard.renew();
        if (canonicalJSON(after.contract) !== canonicalJSON(plan.desired_catch_all) ||
            after.role_routes_sha256 !== before.role_routes_sha256 ||
            after.managed_rules_sha256 !== before.managed_rules_sha256) {
          throw new Error("catch-all post-mutation verification failed");
        }
      } catch (error) {
        verificationError = error;
      }

      let recoveryError;
      if (mutationError || verificationError) {
        try {
          // Enabling failure restores the exact reviewed disabled predecessor.
          // Disable and rollback failures never auto-enable a route.
          const recovery = plan.action === "enable"
            ? before.contract
            : disabledContract(plan.desired_catch_all);
          await leaseGuard.renew();
          await api.replaceCatchAll(recovery);
          await leaseGuard.renew();
          const recovered = catchAllSnapshot(
            await capturePrimaryRoutingState(api, manifest),
          );
          await leaseGuard.renew();
          if (recovered.contract.enabled !== false ||
              canonicalJSON(recovered.contract) !== canonicalJSON(recovery)) {
            throw new Error("catch-all recovery readback failed");
          }
        } catch (error) {
          recoveryError = error;
        }
      }
      const errors = [mutationError, verificationError, recoveryError].filter(Boolean);
      if (errors.length > 1) {
        throw new AggregateError(errors, "catch-all mutation failed and recovery was incomplete");
      }
      if (errors.length === 1) throw errors[0];
      const leaseEvidence = leaseGuard.evidence();
      validateAgentEmailOperationsLeaseEvidence(
        leaseEvidence,
        "catch_all_routing_apply",
      );
      return withReceiptFence(
        receiptBody(plan, fence, before, after, leaseEvidence),
      );
    },
  );
}

export async function inspectCatchAll(api, runtime, manifestInput, {
  now = () => new Date(),
} = {}) {
  const manifest = normalizePrimaryCanaryManifest(manifestInput);
  const routing = await capturePrimaryRoutingState(api, manifest);
  const snapshot = catchAllSnapshot(routing);
  const readiness = await inspectPrimaryReadiness(api, runtime, manifest, { now });
  const contract = snapshot.contract;
  return Object.freeze({
    schema: CATCH_ALL_STATUS_SCHEMA,
    domain: PRIMARY_CANARY_DOMAIN,
    worker: PRIMARY_CANARY_WORKER,
    catch_all: {
      enabled: contract.enabled,
      action: contract.actions[0].type,
      contract_sha256: snapshot.contract_sha256,
      provider_sha256: snapshot.provider_sha256,
    },
    role_routes: {
      ready: snapshot.role_routes_ready,
      sha256: snapshot.role_routes_sha256,
    },
    primary_canary: snapshot.managed_rules,
    readiness: summarizePrimaryReadiness(readiness),
    ready_to_plan_enable:
      !contract.enabled && snapshot.role_routes_ready &&
      canaryIsActive(snapshot, manifest.agents.length) && readiness.activation_ready,
  });
}
