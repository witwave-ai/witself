import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import { isDeepStrictEqual } from "node:util";

import { handleInternalBridgeRequest } from "../src/bridge.mjs";
import {
  DurableAgentEmailDomainRegistry,
  reconcileAgentEmailDomainsForPlan,
} from "../src/agent-email-domain-runtime.mjs";
import {
  DurableRealmEmailAliasRegistry,
  reconcileRealmEmailAliasesForPlan,
} from "../src/realm-email-alias-runtime.mjs";
import { replayAgentEmailDomainJournalPage } from "../src/agent-email-domain-journal.mjs";
import { replayRealmEmailAliasJournalPage } from "../src/realm-email-alias-journal.mjs";

const ACCOUNT = "acc_aaaaaaaaaaaaaaaa";

// Exercise the production authority handlers and their persisted fences. Only
// Durable Object storage and the restored cell transport are in-memory fakes.
class Storage {
  constructor() {
    this.values = new Map();
  }

  async get(key) {
    return structuredClone(this.values.get(key));
  }

  async put(key, value) {
    this.values.set(key, structuredClone(value));
  }

  async delete(key) {
    return this.values.delete(key);
  }

  async list({ prefix = "", limit, reverse = false, startAfter, end } = {}) {
    let entries = [...this.values]
      .filter(([key]) => key.startsWith(prefix))
      .sort(([left], [right]) => left.localeCompare(right));
    if (startAfter) entries = entries.filter(([key]) => key > startAfter);
    if (end) entries = entries.filter(([key]) => key < end);
    if (reverse) entries.reverse();
    if (Number.isSafeInteger(limit)) entries = entries.slice(0, limit);
    return new Map(structuredClone(entries));
  }

  async transaction(callback) {
    const staged = structuredClone(this.values);
    const result = await callback({
      get: async (key) => structuredClone(staged.get(key)),
      put: async (key, value) => staged.set(key, structuredClone(value)),
      delete: async (key) => staged.delete(key),
    });
    this.values = staged;
    return result;
  }

  async setAlarm(value) {
    this.alarm = value;
  }

  async deleteAlarm() {
    this.alarm = undefined;
  }

  async getAlarm() {
    return this.alarm;
  }
}

class JournalBucket {
  constructor() {
    this.values = new Map();
  }

  async put(key, value) {
    if (this.values.has(key)) return null;
    this.values.set(key, new Uint8Array(value));
    return { key };
  }

  async get(key) {
    const value = this.values.get(key);
    return value ? { arrayBuffer: async () => value.slice().buffer } : null;
  }
}

function snapshot(revision, content = "same") {
  const free = content === "free";
  const value = {
    revision,
    plan: free ? "free" : "enterprise",
    limits: free
      ? { agents: 1, operator_seats: 1 }
      : { agents: content === "different" ? 20 : 10 },
    policies: {},
    features: free ? ["facts", "memory"] : [
      "agent_email_custom_domain",
      "agent_email_realm_alias",
    ],
  };
  const ordered = (values) => Object.fromEntries(
    Object.keys(values).sort().map((key) => [key, values[key]]),
  );
  const canonical = JSON.stringify({
    plan: value.plan,
    limits: ordered(value.limits),
    policies: ordered(value.policies),
    features: [...value.features].sort(),
  });
  return {
    ...value,
    snapshot_hash: createHash("sha256").update(canonical).digest("hex"),
  };
}

function cellSnapshot(target) {
  return {
    ...target,
    account_id: ACCOUNT,
    applied_at: "2026-09-19T00:00:00Z",
  };
}

function appliedResult(target) {
  return {
    schema_version: "witself.v0",
    state: "applied",
    account_id: ACCOUNT,
    target_revision: target.revision,
    target_plan: target.plan,
    target_snapshot_hash: target.snapshot_hash,
    violations: [],
    applied_snapshot: cellSnapshot(target),
  };
}

function request(target) {
  return new Request(
    `https://bridge.test/v1/internal/accounts/${ACCOUNT}:plan-fit-apply`,
    {
      method: "POST",
      headers: {
        Authorization: "Bearer test-bridge",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ schema_version: "witself.v0", target }),
    },
  );
}

async function environment(completed, dimensions) {
  const events = [];
  const journals = { domain: new JournalBucket(), alias: new JournalBucket() };
  const directory = new Map([
    [`acct:${ACCOUNT}`, { cell: "restored" }],
    ["cell:restored", {
      endpoint: "https://restored-cell.test",
      provision_token: "test-cell",
    }],
  ]);
  const env = {
    INTERNAL_BRIDGE_TOKEN: "test-bridge",
    AGENT_EMAIL_DOMAIN: "witmail.net",
    CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED: "true",
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED: "true",
    CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST: ACCOUNT,
    AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL: journals.domain,
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED: "true",
    CP_AGENT_EMAIL_DOMAIN_AUTHORITY_STREAM_ID: "aedj_aaaaaaaaaaaaaaaa",
    REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL: journals.alias,
    CP_REALM_EMAIL_ALIAS_AUTHORITY_JOURNAL_ENABLED: "true",
    CP_REALM_EMAIL_ALIAS_AUTHORITY_STREAM_ID: "reaj_aaaaaaaaaaaaaaaa",
    DIRECTORY: {
      get: async (key, options) => {
        assert.equal(options.type, "json");
        return structuredClone(directory.get(key) ?? null);
      },
    },
  };
  const stores = {};
  let currentTime = Date.UTC(2026, 8, 20);
  for (const [label, binding, Runtime] of [
    ["domain", "AGENT_EMAIL_DOMAINS", DurableAgentEmailDomainRegistry],
    ["alias", "REALM_EMAIL_ALIASES", DurableRealmEmailAliasRegistry],
  ]) {
    const storage = new Storage();
    stores[label] = storage;
    const runtime = new Runtime({ storage }, env, {
      now: () => new Date(currentTime++),
      fetch: async () => assert.fail("unexpected authority network request"),
      log: () => {},
    });
    env[binding] = {
      idFromName: (name) => {
        assert.equal(name, "global");
        return name;
      },
      get: () => ({
        fetch: async (url, init) => {
          const body = JSON.parse(init.body);
          events.push(`${label}:${body.mode}:${body.plan_revision}`);
          return runtime.fetch(new Request(url, init));
        },
      }),
    };
  }
  for (const [label, reconcile] of [
    ["domain", reconcileAgentEmailDomainsForPlan],
    ["alias", reconcileRealmEmailAliasesForPlan],
  ]) {
    if (!dimensions.includes(label)) continue;
    const result = await reconcile(env, ACCOUNT, completed, "complete");
    assert.equal(result.complete, true, `${label} initial completion failed`);
    const fence = await stores[label].get(`plan-fence:${ACCOUNT}`);
    assert.equal(fence?.committed_revision, completed.revision);
    assert.equal(fence?.committed_snapshot_hash === completed.snapshot_hash, true);
    assert.equal(await stores[label].get(`plan-intent:${ACCOUNT}`), undefined);
  }
  events.length = 0;
  return { env, stores, events, journals };
}

async function assertJournalReplay(journals, stores) {
  for (const [label, streamID, replay] of [
    ["domain", "aedj_aaaaaaaaaaaaaaaa", replayAgentEmailDomainJournalPage],
    ["alias", "reaj_aaaaaaaaaaaaaaaa", replayRealmEmailAliasJournalPage],
  ]) {
    const entries = [...journals[label].values.values()]
      .map((bytes) => JSON.parse(new TextDecoder().decode(bytes)))
      .sort((left, right) => left.sequence - right.sequence);
    assert.ok(entries.length > 0, `${label} journal must be enabled`);
    const recovered = await replay(entries, { stream_id: streamID });
    assert.equal(recovered.applied, entries.length);
    assert.equal(
      isDeepStrictEqual(recovered.state.get(`plan-fence:${ACCOUNT}`),
        await stores[label].get(`plan-fence:${ACCOUNT}`)),
      true,
      `${label} completed fence must be journal-replayable`,
    );
    assert.equal(recovered.state.has(`plan-intent:${ACCOUNT}`), false);
  }
}

for (const dimensions of [["domain"], ["alias"], ["domain", "alias"]]) {
  for (const content of ["same", "different", "free"]) {
    for (const targetRevision of [18, 19]) {
      test(`restored cell 15 converges with completed ${dimensions.join("+")} 18 and ${content} target ${targetRevision}`, async () => {
        const target = snapshot(targetRevision, content);
        const completed = snapshot(18, content);
        const { env, stores, events, journals } = await environment(completed, dimensions);
        let current = cellSnapshot(snapshot(15, content === "free" ? "free" : "same"));
        let applications = 0;
        const response = await handleInternalBridgeRequest(
          request(target),
          env,
          async (url, init) => {
            if (init.method === "GET") {
              assert.equal(url, `https://restored-cell.test/v1/accounts/${ACCOUNT}:plan`);
              events.push("cell:read");
              return Response.json(current);
            }
            assert.equal(url, `https://restored-cell.test/v1/accounts/${ACCOUNT}:plan-fit-apply`);
            assert.equal(current.revision, 15);
            // Both durable freezes must exist before any cell mutation, even
            // when the target repeats a previously completed authority fence.
            for (const label of ["domain", "alias"]) {
              const pending = await stores[label].get(`plan-intent:${ACCOUNT}`);
              assert.equal(pending?.state, "awaiting_cell");
              assert.equal(pending?.plan_revision, target.revision);
              assert.equal(pending?.plan_snapshot_hash === target.snapshot_hash, true);
              assert.equal(pending?.prepare_fit?.over_limit_count, 0);
              if (dimensions.includes(label)) {
                const committed = await stores[label].get(`plan-fence:${ACCOUNT}`);
                assert.equal(committed?.committed_revision, 18,
                  "preparing a restored cell must not roll completed authority fences backward");
              }
            }
            applications++;
            events.push("cell:apply");
            current = cellSnapshot(target);
            return Response.json(appliedResult(target));
          },
        );
        const result = await response.json();
        assert.equal(response.status, 200, result.error);
        assert.equal(result.state, "applied");
        assert.equal(result.applied_snapshot.revision, targetRevision);
        assert.equal(result.applied_snapshot.snapshot_hash === target.snapshot_hash, true);
        assert.equal(applications, 1);
        assert.deepEqual(events, [
          ...(targetRevision === 18 ? [
            "domain:prepare:18",
            ...(!dimensions.includes("domain") ? ["alias:prepare:18"] : []),
            "cell:read",
          ] : []),
          `domain:prepare:${targetRevision}`,
          `alias:prepare:${targetRevision}`,
          "cell:apply",
          `alias:complete:${targetRevision}`,
          `domain:complete:${targetRevision}`,
        ]);
        for (const label of ["domain", "alias"]) {
          const fence = await stores[label].get(`plan-fence:${ACCOUNT}`);
          assert.equal(fence?.committed_revision, targetRevision);
          assert.equal(fence?.committed_snapshot_hash === target.snapshot_hash, true);
          assert.equal(await stores[label].get(`plan-intent:${ACCOUNT}`), undefined);
        }
        await assertJournalReplay(journals, stores);
      });
    }
  }

  test(`already applied cell 18 completes ${dimensions.join("+")} replay without another fit freeze`, async () => {
    const target = snapshot(18);
    const { env, stores, events, journals } = await environment(target, dimensions);
    const response = await handleInternalBridgeRequest(request(target), env,
      async (url, init) => {
        assert.equal(init.method, "GET", "exact cell replay must not apply again");
        assert.equal(url, `https://restored-cell.test/v1/accounts/${ACCOUNT}:plan`);
        events.push("cell:read");
        return Response.json(cellSnapshot(target));
      });
    const result = await response.json();
    assert.equal(response.status, 200, result.error);
    assert.equal(result.state, "applied");
    assert.deepEqual(events, [
      "domain:prepare:18",
      ...(!dimensions.includes("domain") ? ["alias:prepare:18"] : []),
      "cell:read",
      "alias:complete:18",
      "domain:complete:18",
    ]);
    for (const label of ["domain", "alias"]) {
      assert.equal(await stores[label].get(`plan-intent:${ACCOUNT}`), undefined);
    }
    await assertJournalReplay(journals, stores);
  });
}
