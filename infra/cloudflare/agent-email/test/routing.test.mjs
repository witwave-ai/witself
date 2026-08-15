import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  CONFIG_KEY,
  normalizePilotManifest,
  recipientKey,
  runtimeConfig,
  runtimeRecipient,
} from "../src/directory.mjs";
import {
  activatePilot,
  disablePilot,
  inspectPilot,
  preparePilot,
  removePilot,
} from "../scripts/routing-lib.mjs";
import { EMAIL_DIRECTORY_TITLE } from "../scripts/cloudflare.mjs";

const example = JSON.parse(await readFile(new URL("../pilot.example.json", import.meta.url), "utf8"));
const normalizedExample = normalizePilotManifest(example);
const routesScript = fileURLToPath(new URL("../scripts/routes.mjs", import.meta.url));

class FakeCloudflare {
  constructor() {
    this.namespaceID = "a".repeat(32);
    this.catchAll = {
      id: "f".repeat(32), name: "Catch-all", enabled: true,
      matchers: [{ type: "all" }], actions: [{ type: "forward", value: ["owner@example.com"] }],
      source: "api",
    };
    this.rules = [];
    this.kv = new Map();
    this.settings = { enabled: true, status: "ready", support_subaddress: true };
    this.calls = [];
    this.nextID = 1;
  }
  async getNamespace() { this.calls.push(["getNamespace"]); return { id: this.namespaceID, title: EMAIL_DIRECTORY_TITLE }; }
  async getCatchAll() { this.calls.push(["getCatchAll"]); return structuredClone(this.catchAll); }
  async getEmailRoutingSettings() {
    this.calls.push(["getEmailRoutingSettings"]);
    return structuredClone(this.settings);
  }
  async listRules() { this.calls.push(["listRules"]); return structuredClone(this.rules); }
  async putKV(key, value) { this.calls.push(["putKV", key]); this.kv.set(key, structuredClone(value)); }
  async deleteKV(key) { this.calls.push(["deleteKV", key]); this.kv.delete(key); }
  async createRule(rule) {
    this.calls.push(["createRule"]);
    const created = { ...structuredClone(rule), id: (this.nextID++).toString(16).padStart(32, "0"), priority: this.nextID };
    this.rules.push(created);
    return structuredClone(created);
  }
  async updateRule(id, rule) {
    this.calls.push(["updateRule", id]);
    const index = this.rules.findIndex((item) => item.id === id);
    assert.notEqual(index, -1);
    this.rules[index] = { ...structuredClone(rule), id };
    return structuredClone(this.rules[index]);
  }
  async deleteRule(id) {
    this.calls.push(["deleteRule", id]);
    this.rules = this.rules.filter((item) => item.id !== id);
  }
}

function installLegacyPilot(api, { enabled = true } = {}) {
  api.kv.set(CONFIG_KEY, runtimeConfig(normalizedExample, enabled));
  normalizedExample.agents.forEach((agent, index) => {
    api.kv.set(
      recipientKey(agent.address),
      runtimeRecipient(normalizedExample, agent),
    );
    api.rules.push({
      id: String(index + 1).padStart(32, "0"),
      name: `witself-agent-email-pilot:${agent.address}`,
      enabled,
      matchers: [{ type: "literal", field: "to", value: agent.address }],
      actions: [{ type: "worker", value: ["witself-agent-email-pilot"] }],
      priority: index + 1,
      source: "api",
    });
  });
}

test("legacy prepare and activate are retired before any provider access", async () => {
  for (const operation of [preparePilot, activatePilot]) {
    const api = new FakeCloudflare();
    await assert.rejects(
      () => operation(api, example),
      /legacy literal-route prepare and activate are retired/,
    );
    assert.deepEqual(api.calls, []);
    assert.equal(api.rules.length, 0);
    assert.equal(api.kv.size, 0);
  }
});

test("legacy routes CLI refuses prepare and activate before loading configuration", () => {
  for (const operation of ["prepare", "activate"]) {
    const result = spawnSync(
      process.execPath,
      [routesScript, operation, "/definitely/missing/pilot.json"],
      { encoding: "utf8" },
    );
    assert.equal(result.status, 1, operation);
    assert.equal(result.stdout, "", operation);
    assert.match(
      result.stderr,
      /legacy literal-route prepare and activate are retired/,
      operation,
    );
    assert.doesNotMatch(
      result.stderr,
      /manifest is missing|CLOUDFLARE_ZONE_ID/,
      operation,
    );
  }
});

test("status reads existing legacy resources without mutating them", async () => {
  const api = new FakeCloudflare();
  installLegacyPilot(api);
  const rulesBefore = structuredClone(api.rules);
  const kvBefore = structuredClone(api.kv);

  const result = await inspectPilot(api, example);
  assert.deepEqual(result, {
    realm_id: example.realm_id,
    configured: example.agents.length,
    enabled: example.agents.length,
    expected: example.agents.length,
    support_subaddress: true,
  });
  assert.deepEqual(api.rules, rulesBefore);
  assert.deepEqual(api.kv, kvBefore);
  assert.equal(
    api.calls.some(([name]) => [
      "putKV", "deleteKV", "createRule", "updateRule", "deleteRule",
    ].includes(name)),
    false,
  );
});

test("disable and remove remain cleanup-only operations for legacy resources", async () => {
  const api = new FakeCloudflare();
  installLegacyPilot(api);
  const catchAllBefore = structuredClone(api.catchAll);

  const disabled = await disablePilot(api, example);
  assert.deepEqual(disabled, {
    state: "disabled",
    realm_id: example.realm_id,
    addresses: example.agents.length,
  });
  assert.equal(api.rules.every((rule) => rule.enabled === false), true);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.equal(api.calls.some(([name]) => name === "createRule"), false);

  const removed = await removePilot(api, example);
  assert.deepEqual(removed, {
    state: "removed",
    realm_id: example.realm_id,
    addresses: example.agents.length,
  });
  assert.equal(api.rules.length, 0);
  assert.equal(api.kv.size, 0);
  assert.deepEqual(api.catchAll, catchAllBefore);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("status reports disabled subaddressing without mutating the pilot", async () => {
  const api = new FakeCloudflare();
  api.settings.support_subaddress = false;
  const result = await inspectPilot(api, example);
  assert.equal(result.support_subaddress, false);
  assert.equal(result.configured, 0);
  assert.equal(result.enabled, 0);
  assert.equal(api.kv.size, 0);
  assert.equal(api.rules.length, 0);
});

test("catch-all drift during disable retains a fail-closed legacy state", async () => {
  const api = new FakeCloudflare();
  installLegacyPilot(api);
  const originalGetCatchAll = api.getCatchAll.bind(api);
  let reads = 0;
  api.getCatchAll = async () => {
    const value = await originalGetCatchAll();
    reads++;
    if (reads === 2) value.enabled = false;
    return value;
  };
  await assert.rejects(() => disablePilot(api, example), /catch-all changed/);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.equal(api.rules.every((rule) => !rule.enabled), true);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("partial disable failure retains a config-off, routes-disabled state", async () => {
  const api = new FakeCloudflare();
  installLegacyPilot(api);
  const update = api.updateRule.bind(api);
  let disabledUpdates = 0;
  let failed = false;
  api.updateRule = async (id, rule) => {
    if (!failed && !rule.enabled && ++disabledUpdates === 3) {
      failed = true;
      throw new Error("injected disable failure");
    }
    return update(id, rule);
  };
  await assert.rejects(() => disablePilot(api, example), /injected disable failure/);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.equal(api.rules.every((rule) => !rule.enabled), true);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("partial removal leaves any surviving exact routes disabled behind config-off", async () => {
  const api = new FakeCloudflare();
  installLegacyPilot(api);
  const remove = api.deleteRule.bind(api);
  let deletes = 0;
  api.deleteRule = async (id) => {
    if (++deletes === 3) throw new Error("injected removal failure");
    return remove(id);
  };
  await assert.rejects(() => removePilot(api, example), /injected removal failure/);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.equal(api.rules.every((rule) => !rule.enabled), true);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("manifest enforces one canonical realm and five-to-ten explicit agents", () => {
  assert.throws(() => normalizePilotManifest({ ...example, agents: example.agents.slice(0, 4) }), /5-10/);
  assert.throws(() => normalizePilotManifest({
    ...example,
    agents: [...example.agents, ...example.agents.map((agent, index) => ({
      agent_id: `agent_bbbbbbbbbbbbbbb${index + 2}`,
      address: `extra${index}.${example.realm_label}@${example.domain}`,
    })), { agent_id: "agent_ccccccccccccccc2", address: `eleventh.${example.realm_label}@${example.domain}` }],
  }), /5-10/);
  const crossRealm = structuredClone(example);
  crossRealm.agents[0].address = `alpha.zzzzzzzzzzzzzzzz@${example.domain}`;
  assert.throws(() => normalizePilotManifest(crossRealm), /another realm/);
});

test("manifest agent ids use the exact Go-generated base32 shape", () => {
  assert.doesNotThrow(() => normalizePilotManifest(example));
  for (const agentID of [
    "agent_example0000001",
    "agent_AAAAAAAAAAAAAAA2",
    "agent_aaaaaaaaaaaaaaa0",
    "agent_aaaaaaaaaaaaaa2",
    "agent_aaaaaaaaaaaaaaaa2",
    "agent_aaaaaaaaaaaaaaa2\n",
    "other_aaaaaaaaaaaaaaa2",
  ]) {
    const changed = structuredClone(example);
    changed.agents[0].agent_id = agentID;
    assert.throws(() => normalizePilotManifest(changed), /agent id is invalid/, agentID);
  }
});

test("isolated namespace title is mandatory", async () => {
  const api = new FakeCloudflare();
  installLegacyPilot(api);
  api.getNamespace = async () => ({ id: api.namespaceID, title: "DIRECTORY" });
  await assert.rejects(() => disablePilot(api, example), /non-isolated KV namespace/);
  assert.equal(api.rules.every((rule) => rule.enabled), true);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, true);
});
