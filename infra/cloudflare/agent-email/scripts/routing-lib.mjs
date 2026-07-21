import {
  CONFIG_KEY,
  normalizePilotManifest,
  recipientKey,
  runtimeConfig,
  runtimeRecipient,
} from "../src/directory.mjs";
import { assertIsolatedEmailDirectory } from "./cloudflare.mjs";

const RULE_PREFIX = "witself-agent-email-pilot:";

function stable(value) {
  if (Array.isArray(value)) return value.map(stable);
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stable(value[key])]));
  }
  return value;
}

function catchAllFingerprint(rule) {
  return JSON.stringify(stable(rule ?? null));
}

function ruleName(address) {
  return `${RULE_PREFIX}${address}`;
}

function desiredRule(manifest, address, enabled, priority) {
  const rule = {
    name: ruleName(address),
    enabled,
    matchers: [{ type: "literal", field: "to", value: address }],
    actions: [{ type: "worker", value: [manifest.worker_name] }],
  };
  if (Number.isSafeInteger(priority) && priority >= 0) rule.priority = priority;
  return rule;
}

function literalAddress(rule) {
  const matchers = rule?.matchers;
  if (!Array.isArray(matchers) || matchers.length !== 1) return "";
  const matcher = matchers[0];
  return matcher?.type === "literal" && matcher?.field === "to" && typeof matcher?.value === "string"
    ? matcher.value.toLowerCase()
    : "";
}

function isDesiredAction(rule, manifest) {
  return Array.isArray(rule?.actions) && rule.actions.length === 1 &&
    rule.actions[0]?.type === "worker" &&
    Array.isArray(rule.actions[0]?.value) && rule.actions[0].value.length === 1 &&
    rule.actions[0].value[0] === manifest.worker_name;
}

function indexRules(rules, manifest, { rejectStale = true, skipUnmanaged = false } = {}) {
  if (!Array.isArray(rules)) throw new Error("Cloudflare routing-rule list is invalid");
  const enrolled = new Set(manifest.agents.map((agent) => agent.address));
  const indexed = new Map();
  for (const rule of rules) {
    const address = literalAddress(rule);
    if (address && enrolled.has(address)) {
      if (indexed.has(address)) throw new Error(`duplicate Cloudflare rule exists for ${address}`);
      if (rule.name !== ruleName(address) || !isDesiredAction(rule, manifest)) {
        if (skipUnmanaged) continue;
        throw new Error(`refusing to replace an unmanaged routing rule for ${address}`);
      }
      indexed.set(address, rule);
      continue;
    }
    if (rejectStale && typeof rule?.name === "string" && rule.name.startsWith(RULE_PREFIX)) {
      throw new Error("stale pilot routing rule exists outside the manifest");
    }
  }
  return indexed;
}

function assertCatchAllUnchanged(before, after) {
  if (catchAllFingerprint(before) !== catchAllFingerprint(after)) {
    throw new Error("Cloudflare catch-all changed while managing pilot routes");
  }
}

async function assertSubaddressingEnabled(api) {
  const settings = await api.getEmailRoutingSettings();
  if (settings?.support_subaddress !== true) {
    throw new Error("Cloudflare Email Routing subaddressing is not enabled");
  }
  return settings;
}

async function verifyCatchAllUnchanged(api, before) {
  let after;
  try {
    after = await api.getCatchAll();
  } catch (cause) {
    throw new Error("could not verify the Cloudflare catch-all invariant", { cause });
  }
  assertCatchAllUnchanged(before, after);
}

async function operation(api, fn, { rollback } = {}) {
  await assertIsolatedEmailDirectory(api);
  const catchAllBefore = await api.getCatchAll();
  let result;
  let operationError;
  try {
    result = await fn();
  } catch (error) {
    operationError = error;
  }
  let invariantError;
  try {
    await verifyCatchAllUnchanged(api, catchAllBefore);
  } catch (error) {
    invariantError = error;
  }
  if (!operationError && !invariantError) return result;

  let rollbackError;
  if (rollback) {
    try {
      await rollback();
    } catch (error) {
      rollbackError = error;
    }
  }
  if (rollbackError) {
    throw new AggregateError(
      [operationError, invariantError, rollbackError].filter(Boolean),
      "pilot routing operation failed and fail-closed rollback was incomplete",
    );
  }
  if (operationError && invariantError) {
    throw new AggregateError(
      [operationError, invariantError],
      "pilot routing operation failed and the catch-all invariant could not be confirmed",
    );
  }
  throw operationError ?? invariantError;
}

async function writeDirectoryDetails(api, manifest) {
  for (const agent of manifest.agents) {
    await api.putKV(recipientKey(agent.address), runtimeRecipient(manifest, agent));
  }
}

async function setRuleStates(
  api,
  manifest,
  enabled,
  { requireComplete = false, rejectStale = true, skipUnmanaged = false } = {},
) {
  const rules = await api.listRules();
  const indexed = indexRules(rules, manifest, { rejectStale, skipUnmanaged });
  if (requireComplete && indexed.size !== manifest.agents.length) {
    throw new Error("pilot routes are incomplete; run prepare before activate");
  }
  for (const agent of manifest.agents) {
    const current = indexed.get(agent.address);
    if (!current) continue;
    if (current.enabled !== enabled) {
      await api.updateRule(current.id, desiredRule(manifest, agent.address, enabled, current.priority));
    }
  }
  return indexed;
}

async function failClosed(api, manifest) {
  const failures = [];
  try {
    await api.putKV(CONFIG_KEY, runtimeConfig(manifest, false));
  } catch (error) {
    failures.push(error);
  }
  try {
    await setRuleStates(api, manifest, false, { rejectStale: false, skipUnmanaged: true });
  } catch (error) {
    failures.push(error);
  }
  if (failures.length > 0) {
    throw new AggregateError(failures, "pilot fail-closed rollback was incomplete");
  }
}

export async function preparePilot(api, manifestInput) {
  const manifest = normalizePilotManifest(manifestInput);
  await assertSubaddressingEnabled(api);
  const rollback = () => failClosed(api, manifest);
  return operation(api, async () => {
    // Validate before mutating, then make config-off the first write. Any
    // subsequent partial preparation is non-accepting even if a prior route
    // was active.
    indexRules(await api.listRules(), manifest);
    await api.putKV(CONFIG_KEY, runtimeConfig(manifest, false));
    const indexed = await setRuleStates(api, manifest, false);
    await writeDirectoryDetails(api, manifest);
    for (const agent of manifest.agents) {
      if (!indexed.has(agent.address)) {
        await api.createRule(desiredRule(manifest, agent.address, false));
      }
    }
    return { state: "prepared", realm_id: manifest.realm_id, addresses: manifest.agents.length };
  }, { rollback });
}

export async function activatePilot(api, manifestInput) {
  const manifest = normalizePilotManifest(manifestInput);
  await assertSubaddressingEnabled(api);
  const rollback = () => failClosed(api, manifest);
  return operation(api, async () => {
    // Validate the complete exact-route set before exposing an enabled config.
    await setRuleStates(api, manifest, false, { requireComplete: true });
    // Recipient detail rows were published by prepare and need time to reach
    // the edge. Activation changes only the small config gate; rewriting every
    // detail row here would restart their eventual-consistency window.
    await api.putKV(CONFIG_KEY, runtimeConfig(manifest, true));
    await setRuleStates(api, manifest, true, { requireComplete: true });
    return { state: "active", realm_id: manifest.realm_id, addresses: manifest.agents.length };
  }, { rollback });
}

export async function disablePilot(api, manifestInput) {
  const manifest = normalizePilotManifest(manifestInput);
  const rollback = () => failClosed(api, manifest);
  return operation(api, async () => {
    await api.putKV(CONFIG_KEY, runtimeConfig(manifest, false));
    await setRuleStates(api, manifest, false);
    return { state: "disabled", realm_id: manifest.realm_id, addresses: manifest.agents.length };
  }, { rollback });
}

export async function removePilot(api, manifestInput) {
  const manifest = normalizePilotManifest(manifestInput);
  const rollback = () => failClosed(api, manifest);
  return operation(api, async () => {
    await api.putKV(CONFIG_KEY, runtimeConfig(manifest, false));
    await setRuleStates(api, manifest, false);
    const indexed = indexRules(await api.listRules(), manifest);
    for (const agent of manifest.agents) {
      const rule = indexed.get(agent.address);
      if (rule) await api.deleteRule(rule.id);
    }
    for (const agent of manifest.agents) await api.deleteKV(recipientKey(agent.address));
    await api.deleteKV(CONFIG_KEY);
    return { state: "removed", realm_id: manifest.realm_id, addresses: manifest.agents.length };
  }, { rollback });
}

export async function inspectPilot(api, manifestInput) {
  const manifest = normalizePilotManifest(manifestInput);
  const settings = await api.getEmailRoutingSettings();
  return operation(api, async () => {
    const indexed = indexRules(await api.listRules(), manifest);
    return {
      realm_id: manifest.realm_id,
      configured: indexed.size,
      enabled: manifest.agents.filter((agent) => indexed.get(agent.address)?.enabled === true).length,
      expected: manifest.agents.length,
      support_subaddress: settings.support_subaddress,
    };
  });
}

export const routingInternals = { catchAllFingerprint, desiredRule, failClosed, indexRules, ruleName };
