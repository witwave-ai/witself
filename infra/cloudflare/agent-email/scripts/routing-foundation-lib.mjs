import { createHash } from "node:crypto";

import {
  validateAgentEmailOperationsLeaseEvidence,
} from "../../control-plane/src/agent-email-operations-lease.mjs";
import {
  LEGACY_PILOT_WORKER,
  PRODUCTION_RECEIVE_WORKER,
} from "../src/worker-names.mjs";
import {
  primaryRoutingInternals,
} from "./primary-routing-lib.mjs";

export const ROUTING_FOUNDATION_DOMAIN = "witmail.net";
export const ROUTING_FOUNDATION_OPERATION =
  "email_routing_settings_apply";
export const ROUTING_FOUNDATION_PLAN_SCHEMA =
  "witself.agent-email-routing-foundation-plan.v1";
export const ROUTING_FOUNDATION_RECEIPT_SCHEMA =
  "witself.agent-email-routing-foundation-receipt.v1";
export const ROUTING_FOUNDATION_STATUS_SCHEMA =
  "witself.agent-email-routing-foundation-status.v1";

const PLAN_LIFETIME_MS = 15 * 60 * 1_000;
const ACTIONS = new Set(["enable", "disable"]);
const ID = /^[0-9a-f]{32}(?![\s\S])/;
const SHA256 = /^[0-9a-f]{64}(?![\s\S])/;
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

function normalizeZone(zone, api) {
  if (!zone || typeof zone !== "object" || Array.isArray(zone) ||
      zone.id !== api.zoneID || zone.account?.id !== api.accountID ||
      zone.name !== ROUTING_FOUNDATION_DOMAIN || zone.status !== "active") {
    throw new Error("Cloudflare routing foundation zone identity was invalid");
  }
  return Object.freeze({
    account_id: api.accountID,
    zone_id: api.zoneID,
    domain: ROUTING_FOUNDATION_DOMAIN,
    active: true,
  });
}

function normalizeSettings(settings, api) {
  if (!settings || typeof settings !== "object" || Array.isArray(settings) ||
      settings.id !== api.zoneID ||
      settings.name !== ROUTING_FOUNDATION_DOMAIN ||
      typeof settings.enabled !== "boolean" ||
      typeof settings.skip_wizard !== "boolean" ||
      typeof settings.support_subaddress !== "boolean" ||
      typeof settings.status !== "string" || settings.status.length < 1 ||
      settings.status.length > 64) {
    throw new Error("Cloudflare Email Routing settings were invalid");
  }
  return Object.freeze({
    contract: Object.freeze({
      enabled: settings.enabled,
      skip_wizard: settings.skip_wizard,
      support_subaddress: settings.support_subaddress,
    }),
    status: settings.status,
    provider_sha256: sha256(canonicalJSON(settings)),
  });
}

function ruleTargetsWorker(rule, worker) {
  return Array.isArray(rule?.actions) && rule.actions.some((action) =>
    action?.type === "worker" && Array.isArray(action.value) &&
    action.value.includes(worker));
}

function normalizeRuleInventory(rules) {
  if (!Array.isArray(rules)) {
    throw new Error("Cloudflare Email Routing rule inventory was invalid");
  }
  const ids = new Set();
  for (const rule of rules) {
    if (!rule || typeof rule !== "object" || Array.isArray(rule) ||
        typeof rule.id !== "string" || rule.id.length < 1 ||
        rule.id.length > 32 || !/^[0-9a-f]+$/.test(rule.id) ||
        ids.has(rule.id) || typeof rule.enabled !== "boolean" ||
        !Array.isArray(rule.matchers) || !Array.isArray(rule.actions)) {
      throw new Error("Cloudflare Email Routing rule inventory was invalid");
    }
    for (const action of rule.actions) {
      if (action?.type === "worker" &&
          (!Array.isArray(action.value) || action.value.length < 1 ||
            action.value.some((value) =>
              typeof value !== "string" || value.length < 1 ||
              value.length > 255))) {
        throw new Error("Cloudflare Email Routing rule inventory was invalid");
      }
    }
    ids.add(rule.id);
  }
  const ordered = [...rules].sort((left, right) =>
    left.id.localeCompare(right.id));
  const production = ordered.filter((rule) =>
    ruleTargetsWorker(rule, PRODUCTION_RECEIVE_WORKER));
  const retired = ordered.filter((rule) =>
    ruleTargetsWorker(rule, LEGACY_PILOT_WORKER));
  const targeted = [...new Set([...production, ...retired])];
  return Object.freeze({
    total: ordered.length,
    sha256: sha256(canonicalJSON(ordered)),
    witself_worker_targeted: targeted.length,
    witself_worker_targeted_enabled:
      targeted.filter((rule) => rule.enabled).length,
    production_worker_targeted: production.length,
    production_worker_targeted_enabled:
      production.filter((rule) => rule.enabled).length,
    retired_worker_targeted: retired.length,
    retired_worker_targeted_enabled:
      retired.filter((rule) => rule.enabled).length,
  });
}

export async function captureRoutingFoundationState(api) {
  if (!api || !ID.test(String(api.accountID ?? "")) ||
      !ID.test(String(api.zoneID ?? "")) ||
      typeof api.getZone !== "function" ||
      typeof api.getEmailRoutingSettings !== "function" ||
      typeof api.getCatchAll !== "function" ||
      typeof api.listRules !== "function") {
    throw new Error("routing foundation Cloudflare API was invalid");
  }
  const [zone, settings, catchAll, rules] = await Promise.all([
    api.getZone(),
    api.getEmailRoutingSettings(),
    api.getCatchAll(),
    api.listRules(),
  ]);
  const zoneIdentity = normalizeZone(zone, api);
  const emailRouting = normalizeSettings(settings, api);
  const catchAllSummary = primaryRoutingInternals.catchAllState(catchAll);
  const roleRoutes = primaryRoutingInternals.roleRouteState(
    rules,
    ROUTING_FOUNDATION_DOMAIN,
  );
  const routingRules = normalizeRuleInventory(rules);
  return Object.freeze({
    summary: Object.freeze({
      zone: Object.freeze({
        ...zoneIdentity,
        sha256: sha256(canonicalJSON(zoneIdentity)),
      }),
      email_routing: emailRouting,
      catch_all: catchAllSummary,
      role_routes: roleRoutes,
      routing_rules: routingRules,
    }),
  });
}

function enableReady(summary) {
  return summary.zone.active === true &&
    summary.email_routing.contract.enabled === true &&
    summary.email_routing.status === "ready" &&
    summary.email_routing.contract.support_subaddress === false &&
    summary.catch_all.enabled === false &&
    summary.role_routes.ready === true &&
    summary.routing_rules.witself_worker_targeted === 0;
}

function disableReady(summary) {
  return summary.zone.active === true &&
    summary.email_routing.contract.enabled === true &&
    summary.email_routing.contract.support_subaddress === true &&
    summary.routing_rules.witself_worker_targeted_enabled === 0;
}

function assertActionPreconditions(action, summary) {
  if (action === "enable" && !enableReady(summary)) {
    throw new Error("Email Routing subaddressing enable foundation was not ready");
  }
  if (action === "disable" && !disableReady(summary)) {
    throw new Error("Email Routing subaddressing disable foundation was not ready");
  }
}

function desiredSettings(summary, action) {
  return Object.freeze({
    ...summary.email_routing.contract,
    support_subaddress: action === "enable",
  });
}

function planBody(api, action, state, createdAt) {
  return {
    schema: ROUTING_FOUNDATION_PLAN_SCHEMA,
    action,
    domain: ROUTING_FOUNDATION_DOMAIN,
    created_at: createdAt.toISOString(),
    expires_at: new Date(
      createdAt.valueOf() + PLAN_LIFETIME_MS,
    ).toISOString(),
    target: {
      account_id: api.accountID,
      zone_id: api.zoneID,
    },
    precondition: state.summary,
    desired_settings: desiredSettings(state.summary, action),
    checks: [
      "target_ids_exact",
      "shared_durable_operations_lease",
      "zone_identity_exact",
      "email_routing_provider_fingerprint_exact",
      "catch_all_fingerprint_preserved",
      "operator_role_fingerprint_preserved",
      "routing_rule_inventory_fingerprint_preserved",
      "settings_readback_exact",
      ...(action === "enable"
        ? [
          "email_routing_ready",
          "catch_all_disabled",
          "operator_roles_ready",
          "worker_routes_absent",
        ]
        : ["fail_closed_subaddressing_disable"]),
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

export async function inspectRoutingFoundation(api) {
  const state = await captureRoutingFoundationState(api);
  return Object.freeze({
    schema: ROUTING_FOUNDATION_STATUS_SCHEMA,
    domain: ROUTING_FOUNDATION_DOMAIN,
    routing: state.summary,
    ready_to_enable_subaddressing: enableReady(state.summary),
    ready_to_disable_subaddressing: disableReady(state.summary),
  });
}

export async function createRoutingFoundationPlan(
  api,
  action,
  { now = () => new Date(), createdAt = null } = {},
) {
  if (!ACTIONS.has(action)) {
    throw new Error("routing foundation action was invalid");
  }
  const clock = now();
  if (!(clock instanceof Date) || !Number.isFinite(clock.valueOf())) {
    throw new Error("routing foundation planner clock was invalid");
  }
  const creation = createdAt === null ? clock : new Date(createdAt);
  if (!Number.isFinite(creation.valueOf())) {
    throw new Error("routing foundation plan creation time was invalid");
  }
  const state = await captureRoutingFoundationState(api);
  assertActionPreconditions(action, state.summary);
  return withFence(planBody(api, action, state, creation));
}

export function verifyRoutingFoundationPlan(
  plan,
  suppliedSHA256,
  { now = () => new Date() } = {},
) {
  exactKeys(plan, [
    "schema", "action", "domain", "created_at", "expires_at", "target",
    "precondition", "desired_settings", "checks", "apply_fence",
  ], "routing foundation plan");
  exactKeys(
    plan.target,
    ["account_id", "zone_id"],
    "routing foundation target",
  );
  exactKeys(
    plan.desired_settings,
    ["enabled", "skip_wizard", "support_subaddress"],
    "routing foundation desired settings",
  );
  exactKeys(
    plan.apply_fence,
    ["algorithm", "sha256"],
    "routing foundation apply fence",
  );
  const clock = now();
  if (plan.schema !== ROUTING_FOUNDATION_PLAN_SCHEMA ||
      !ACTIONS.has(plan.action) ||
      plan.domain !== ROUTING_FOUNDATION_DOMAIN ||
      !RFC3339_UTC.test(String(plan.created_at ?? "")) ||
      !RFC3339_UTC.test(String(plan.expires_at ?? "")) ||
      Date.parse(plan.expires_at) !==
        Date.parse(plan.created_at) + PLAN_LIFETIME_MS ||
      !(clock instanceof Date) || !Number.isFinite(clock.valueOf()) ||
      Date.parse(plan.created_at) > clock.valueOf() + 300_000 ||
      clock.valueOf() > Date.parse(plan.expires_at) ||
      !ID.test(String(plan.target.account_id ?? "")) ||
      !ID.test(String(plan.target.zone_id ?? "")) ||
      typeof plan.desired_settings.enabled !== "boolean" ||
      typeof plan.desired_settings.skip_wizard !== "boolean" ||
      plan.desired_settings.support_subaddress !==
        (plan.action === "enable") ||
      !Array.isArray(plan.checks) || plan.checks.length < 8 ||
      plan.apply_fence.algorithm !== "sha256" ||
      !SHA256.test(String(plan.apply_fence.sha256 ?? ""))) {
    throw new Error("routing foundation plan was malformed or expired");
  }
  const { apply_fence: ignored, ...body } = plan;
  const calculated = sha256(canonicalJSON(body));
  if (calculated !== plan.apply_fence.sha256) {
    throw new Error("routing foundation plan fence did not match its content");
  }
  if (!SHA256.test(String(suppliedSHA256 ?? "")) ||
      suppliedSHA256 !== calculated) {
    throw new Error(
      "--plan-sha256 did not match the exact routing foundation plan",
    );
  }
  return calculated;
}

function exactPlan(actual, expected) {
  if (canonicalJSON(actual) !== canonicalJSON(expected)) {
    throw new Error(
      "routing foundation plan preconditions changed; create and review a new plan",
    );
  }
}

function providerGuards(summary) {
  return Object.freeze({
    zone_sha256: summary.zone.sha256,
    catch_all_sha256: summary.catch_all.sha256,
    role_routes_sha256: summary.role_routes.sha256,
    routing_rules_sha256: summary.routing_rules.sha256,
  });
}

function guardsEqual(left, right) {
  return canonicalJSON(providerGuards(left)) ===
    canonicalJSON(providerGuards(right));
}

function settingsEqual(left, right) {
  return canonicalJSON(left) === canonicalJSON(right);
}

function assertPostcondition(plan, before, after) {
  if (!settingsEqual(after.email_routing.contract, plan.desired_settings) ||
      after.email_routing.status !== before.email_routing.status ||
      !guardsEqual(before, after)) {
    throw new Error("routing foundation postcondition was not exact");
  }
}

async function editSettings(api, settings, leaseGuard) {
  await leaseGuard.renew();
  await api.editEmailRoutingSettings(settings);
  await leaseGuard.renew();
}

async function failClosedAfterEnable(api, before, leaseGuard) {
  await editSettings(api, before.email_routing.contract, leaseGuard);
  const restored = await captureRoutingFoundationState(api);
  if (!settingsEqual(
    restored.summary.email_routing.contract,
    before.email_routing.contract,
  )) {
    throw new Error("routing foundation fail-closed restoration was incomplete");
  }
}

function receiptBody(plan, fence, before, after, leaseEvidence) {
  return {
    schema: ROUTING_FOUNDATION_RECEIPT_SCHEMA,
    outcome: "verified",
    action: plan.action,
    domain: plan.domain,
    plan_sha256: fence,
    target: plan.target,
    operations_lease: leaseEvidence,
    before_settings: before.email_routing.contract,
    after_settings: after.email_routing.contract,
    provider_fingerprints: {
      before_settings_sha256: before.email_routing.provider_sha256,
      after_settings_sha256: after.email_routing.provider_sha256,
      ...providerGuards(after),
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

export function verifyRoutingFoundationReceipt(receipt) {
  exactKeys(receipt, [
    "schema", "outcome", "action", "domain", "plan_sha256", "target",
    "operations_lease", "before_settings", "after_settings",
    "provider_fingerprints", "receipt_fence",
  ], "routing foundation receipt");
  exactKeys(
    receipt.target,
    ["account_id", "zone_id"],
    "routing foundation receipt target",
  );
  exactKeys(receipt.provider_fingerprints, [
    "before_settings_sha256", "after_settings_sha256", "zone_sha256",
    "catch_all_sha256", "role_routes_sha256", "routing_rules_sha256",
  ], "routing foundation provider fingerprints");
  exactKeys(
    receipt.receipt_fence,
    ["algorithm", "sha256"],
    "routing foundation receipt fence",
  );
  for (const settings of [receipt.before_settings, receipt.after_settings]) {
    exactKeys(
      settings,
      ["enabled", "skip_wizard", "support_subaddress"],
      "routing foundation receipt settings",
    );
    if (typeof settings.enabled !== "boolean" ||
        typeof settings.skip_wizard !== "boolean" ||
        typeof settings.support_subaddress !== "boolean") {
      throw new Error("routing foundation receipt settings were malformed");
    }
  }
  if (receipt.schema !== ROUTING_FOUNDATION_RECEIPT_SCHEMA ||
      receipt.outcome !== "verified" || !ACTIONS.has(receipt.action) ||
      receipt.domain !== ROUTING_FOUNDATION_DOMAIN ||
      !SHA256.test(String(receipt.plan_sha256 ?? "")) ||
      !ID.test(String(receipt.target.account_id ?? "")) ||
      !ID.test(String(receipt.target.zone_id ?? "")) ||
      !Object.values(receipt.provider_fingerprints)
        .every((value) => SHA256.test(String(value ?? ""))) ||
      receipt.before_settings.enabled !== receipt.after_settings.enabled ||
      receipt.before_settings.skip_wizard !==
        receipt.after_settings.skip_wizard ||
      receipt.before_settings.support_subaddress !==
        (receipt.action === "disable") ||
      receipt.after_settings.support_subaddress !==
        (receipt.action === "enable") ||
      receipt.receipt_fence.algorithm !== "sha256" ||
      !SHA256.test(String(receipt.receipt_fence.sha256 ?? ""))) {
    throw new Error("routing foundation receipt was malformed");
  }
  validateAgentEmailOperationsLeaseEvidence(
    receipt.operations_lease,
    ROUTING_FOUNDATION_OPERATION,
  );
  const { receipt_fence: ignored, ...body } = receipt;
  if (sha256(canonicalJSON(body)) !== receipt.receipt_fence.sha256) {
    throw new Error("routing foundation receipt fence was invalid");
  }
  return structuredClone(receipt);
}

export async function applyRoutingFoundationPlan(
  plan,
  suppliedSHA256,
  api,
  runtime,
  { now = () => new Date() } = {},
) {
  const fence = verifyRoutingFoundationPlan(plan, suppliedSHA256, { now });
  if (!runtime?.operationsLease ||
      typeof runtime.operationsLease.run !== "function" ||
      typeof api?.editEmailRoutingSettings !== "function") {
    throw new Error("routing foundation mutation runtime was invalid");
  }
  return runtime.operationsLease.run(
    ROUTING_FOUNDATION_OPERATION,
    async (leaseGuard) => {
      const reviewed = await createRoutingFoundationPlan(api, plan.action, {
        now,
        createdAt: plan.created_at,
      });
      exactPlan(reviewed, plan);
      const before = await captureRoutingFoundationState(api);
      if (canonicalJSON(before.summary) !==
          canonicalJSON(plan.precondition)) {
        throw new Error(
          "routing foundation state changed immediately before mutation",
        );
      }

      let mutationError;
      let verificationError;
      let recoveryError;
      let after;
      try {
        await editSettings(api, plan.desired_settings, leaseGuard);
      } catch (error) {
        mutationError = error;
      }
      if (!mutationError) {
        try {
          after = await captureRoutingFoundationState(api);
          assertPostcondition(plan, before.summary, after.summary);
        } catch (error) {
          verificationError = error;
        }
      }
      if ((mutationError || verificationError) && plan.action === "enable") {
        try {
          await failClosedAfterEnable(api, before.summary, leaseGuard);
        } catch (error) {
          recoveryError = error;
        }
      }
      const errors = [mutationError, verificationError, recoveryError]
        .filter(Boolean);
      if (errors.length > 1) {
        throw new AggregateError(
          errors,
          "routing foundation mutation failed and restoration was incomplete",
        );
      }
      if (errors.length === 1) throw errors[0];

      const leaseEvidence = leaseGuard.evidence();
      validateAgentEmailOperationsLeaseEvidence(
        leaseEvidence,
        ROUTING_FOUNDATION_OPERATION,
      );
      return withReceiptFence(receiptBody(
        plan,
        fence,
        before.summary,
        after.summary,
        leaseEvidence,
      ));
    },
  );
}

export const routingFoundationInternals = Object.freeze({
  canonicalJSON,
  normalizeRuleInventory,
  normalizeSettings,
  providerGuards,
  sha256,
});
