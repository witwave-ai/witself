import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { CONFIG_KEY, normalizePilotManifest } from "../src/directory.mjs";
import { activatePilot, disablePilot, preparePilot, removePilot } from "../scripts/routing-lib.mjs";
import { EMAIL_DIRECTORY_TITLE } from "../scripts/cloudflare.mjs";

const example = JSON.parse(await readFile(new URL("../pilot.example.json", import.meta.url), "utf8"));

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
    this.calls = [];
    this.nextID = 1;
  }
  async getNamespace() { this.calls.push(["getNamespace"]); return { id: this.namespaceID, title: EMAIL_DIRECTORY_TITLE }; }
  async getCatchAll() { this.calls.push(["getCatchAll"]); return structuredClone(this.catchAll); }
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

test("prepare creates only disabled literal rules and an isolated config", async () => {
  const api = new FakeCloudflare();
  const before = structuredClone(api.catchAll);
  const result = await preparePilot(api, example);
  assert.deepEqual(result, { state: "prepared", realm_id: example.realm_id, addresses: 5 });
  assert.equal(api.rules.length, 5);
  for (const rule of api.rules) {
    assert.equal(rule.enabled, false);
    assert.deepEqual(rule.matchers, [{ type: "literal", field: "to", value: rule.name.split(":").slice(1).join(":") }]);
    assert.deepEqual(rule.actions, [{ type: "worker", value: ["witself-agent-email-pilot"] }]);
  }
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.deepEqual(api.catchAll, before);
  assert.equal(api.calls.filter(([name]) => name === "getCatchAll").length, 2);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("activate, disable, and remove are exact-route lifecycle operations", async () => {
  const api = new FakeCloudflare();
  await preparePilot(api, example);
  await activatePilot(api, example);
  assert.equal(api.rules.every((rule) => rule.enabled), true);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, true);

  await disablePilot(api, example);
  assert.equal(api.rules.every((rule) => !rule.enabled), true);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);

  const result = await removePilot(api, example);
  assert.equal(result.state, "removed");
  assert.equal(api.rules.length, 0);
  assert.equal(api.kv.size, 0);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("activation refuses incomplete or unmanaged address rules", async () => {
  const incomplete = new FakeCloudflare();
  await assert.rejects(() => activatePilot(incomplete, example), /run prepare/);

  const conflict = new FakeCloudflare();
  const manifest = normalizePilotManifest(example);
  conflict.rules.push({
    id: "1".repeat(32), name: "other", enabled: true,
    matchers: [{ type: "literal", field: "to", value: manifest.agents[0].address }],
    actions: [{ type: "forward", value: ["owner@example.com"] }],
  });
  await assert.rejects(() => preparePilot(conflict, example), /unmanaged routing rule/);
});

test("partial activation failure rolls the config and exact rules back to disabled", async () => {
  const api = new FakeCloudflare();
  await preparePilot(api, example);
  const update = api.updateRule.bind(api);
  let enabledUpdates = 0;
  api.updateRule = async (id, rule) => {
    if (rule.enabled && ++enabledUpdates === 3) throw new Error("injected route failure");
    return update(id, rule);
  };
  await assert.rejects(() => activatePilot(api, example), /injected route failure/);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.equal(api.rules.every((rule) => !rule.enabled), true);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("catch-all drift detected after activation triggers fail-closed rollback", async () => {
  const api = new FakeCloudflare();
  await preparePilot(api, example);
  const originalGetCatchAll = api.getCatchAll.bind(api);
  let reads = 0;
  api.getCatchAll = async () => {
    const value = await originalGetCatchAll();
    reads++;
    if (reads === 2) value.enabled = false;
    return value;
  };
  await assert.rejects(() => activatePilot(api, example), /catch-all changed/);
  assert.equal(api.kv.get(CONFIG_KEY).enabled, false);
  assert.equal(api.rules.every((rule) => !rule.enabled), true);
  assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
});

test("partial prepare and disable failures retain a config-off, routes-disabled state", async () => {
  for (const operation of ["prepare", "disable"]) {
    const api = new FakeCloudflare();
    await preparePilot(api, example);
    await activatePilot(api, example);
    if (operation === "prepare") {
      const put = api.putKV.bind(api);
      let failed = false;
      api.putKV = async (key, value) => {
        if (!failed && key !== CONFIG_KEY) {
          failed = true;
          throw new Error("injected detail failure");
        }
        return put(key, value);
      };
      await assert.rejects(() => preparePilot(api, example), /injected detail failure/);
    } else {
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
    }
    assert.equal(api.kv.get(CONFIG_KEY).enabled, false, operation);
    assert.equal(api.rules.every((rule) => !rule.enabled), true, operation);
    assert.equal(api.calls.some(([name]) => name === "updateCatchAll"), false);
  }
});

test("partial removal leaves any surviving exact routes disabled behind config-off", async () => {
  const api = new FakeCloudflare();
  await preparePilot(api, example);
  await activatePilot(api, example);
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
  api.getNamespace = async () => ({ id: api.namespaceID, title: "DIRECTORY" });
  await assert.rejects(() => preparePilot(api, example), /non-isolated KV namespace/);
  assert.equal(api.rules.length, 0);
});
