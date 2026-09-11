// Public Worker + actual AccountSignup wrapper rehearsal. These closed local
// services attest fixture relationships, not provider persistence or delivery.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { register } from "node:module";
import { isDeepStrictEqual } from "node:util";

register(new URL("./cloudflare-containers-loader.mjs", import.meta.url));
register(new URL("./signup-b2-legal-loader.mjs", import.meta.url));
const { AccountSignup, default: worker } = await import("../../src/index.js");
const { default: legal } = await import("../../../../../web/legal/index.js");
const { signupIPScope } = await import("../../src/signup-counters.mjs");
const { containerCalls, resetContainerCalls } = await import("./cloudflare-containers-stub.mjs");

export const B2 = Object.freeze({
  origin: "https://cp.b2.invalid", cellOrigin: "https://cell.b2.invalid",
  provision: "b2-original", candidate: "b2-successor", transition: "b2-transition",
  email: "owner@example.test", displayName: "B2 Fixture", invite: "b2-invite",
  challenge: "b2-fixture-challenge", sourceIP: "203.0.113.42",
});
const CELL = "b2-cell";
const REGISTRATION = "b2-registration";
const ACCOUNT = "acct_b2_fixture";
const OPERATOR = "opr_b2_fixture";
const PROVISION_TOKEN = "b2-fake-provision-token";
const TURNSTILE_SECRET = "b2-fake-turnstile-secret";
const STATE_KEY = "account-signup";
const LEGAL_PHASES = new Set(["legal_pending", "legal_rejected", "legal_reserved"]);
let activeFixture = false;

// Cloned values and staged, serialized transactions match the existing abuse
// fixture. Re-instantiation retains these Maps, never a production store.
class Storage {
  constructor(check, changed) {
    this.values = new Map();
    this.queue = Promise.resolve();
    this.check = check;
    this.changed = changed;
  }

  async get(key) { return structuredClone(this.values.get(key)); }
  async put(key, value) {
    this.check(typeof key === "string" && key.length <= 256 &&
      JSON.stringify(value).length <= 32768 && this.values.size < 32, "storage bound");
    this.values.set(key, structuredClone(value));
    this.changed(key, value);
  }
  async delete(key) { this.values.delete(key); }
  async list({ prefix = "" } = {}) {
    return new Map([...this.values].filter(([key]) => key.startsWith(prefix))
      .map(([key, value]) => [key, structuredClone(value)]));
  }
  transaction(callback) {
    const work = async () => {
      const staged = new Storage(this.check, () => {});
      staged.values = structuredClone(this.values);
      const result = await callback(staged);
      this.values = staged.values;
      return result;
    };
    const result = this.queue.then(work, work);
    this.queue = result.catch(() => {});
    return result;
  }
}

export async function makeSignupB2Fixture(options) {
  assert.equal(activeFixture, false, "B2 fixtures must run sequentially");
  const cliMode = options !== undefined;
  if (cliMode) {
    assert.deepEqual(Object.keys(options), ["cliOrigin"], "closed CLI fixture options");
    assert.match(options.cliOrigin, /^http:\/\/127\.0\.0\.1:[1-9][0-9]{0,4}$/, "numeric loopback origin");
    const parsed = new URL(options.cliOrigin);
    assert.equal(parsed.origin, options.cliOrigin, "canonical loopback origin");
  }
  const origin = cliMode ? options.cliOrigin : B2.origin;
  const cellOrigin = cliMode ? origin : B2.cellOrigin;
  const accountID = cliMode ? "acc_abcdefghijklmnop" : ACCOUNT;
  const operatorID = cliMode ? "opr_abcdefghijklmnop" : OPERATOR;
  let originalID = cliMode ? null : B2.provision;
  let candidateID = cliMode ? null : B2.candidate;
  let transitionID = cliMode ? null : B2.transition;
  const cliCounts = { manifest: 0, bootstrap: 0 };
  let lastBootstrapToken = null;
  let claimedBootstrap = false;
  const originalFetch = globalThis.fetch;
  const violations = [];
  const check = (condition, code) => {
    if (condition) return;
    if (violations.length < 64) violations.push(code);
    throw new Error(`B2 fixture boundary: ${code}`);
  };
  const exact = (value, keys, code) => check(value !== null &&
    typeof value === "object" && !Array.isArray(value) &&
    Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key)), code);
  const urlOf = (request) => {
    let url;
    try { url = new URL(request.url); }
    catch { check(false, "invalid URL"); }
    check(!url.username && !url.password && !url.search && !url.hash, "URL shape");
    return url;
  };
  const jsonOf = async (request) => {
    try { return await request.json(); }
    catch { check(false, "invalid fixture JSON"); }
  };
  const counts = {
    publicSignup: 0, publicReconsent: 0, publicLimiter: 0, signupLimiter: 0,
    canonical: 0, turnstile: 0, counters: 0, invite: 0, placement: 0,
    protocol: 0, provision: 0, reserve: 0, email: 0, event: 0,
    targetBegin: 0, targetAttach: 0, targetPromote: 0, droppedOuterAck: 0,
  };
  const phaseEvents = [];
  const generatedLegalStates = new Map();
  const registrationHashes = [];
  const storages = new Map();
  const objects = new Map();
  const receipts = new Map();
  const targets = new Map();
  const verificationHashes = new Set();
  const tasks = [];
  let authorityAvailable = true;
  let dropOuterAck = false;
  let closed = false;
  let bootstrapSequence = 0;
  const ipScope = await signupIPScope(B2.sourceIP);
  const roles = new Map([
    [`invite:${B2.invite}`, "invite"], [ipScope, "ip"], ["signup-counter:global", "global"],
  ]);
  if (!cliMode) {
    roles.set(`provision:${originalID}`, "original");
    roles.set(`provision:${candidateID}`, "candidate");
  }
  // One direct setup read supplies expected labels from the unchanged handler;
  // canonical binding reads are counted separately and always use that handler.
  const manifest = await legal.fetch(new Request("https://legal.internal/legal/versions.json")).json();
  const currentPair = Object.freeze({
    consent_terms_version: manifest.terms.version,
    consent_privacy_version: manifest.privacy.version,
  });
  const stalePair = Object.freeze({
    consent_terms_version: "b2-synthetic-prior-terms",
    consent_privacy_version: "b2-synthetic-prior-privacy",
  });
  check(currentPair.consent_terms_version !== stalePair.consent_terms_version &&
    currentPair.consent_privacy_version !== stalePair.consent_privacy_version, "synthetic prior labels");
  const cell = {
    name: CELL, endpoint: cellOrigin, cloud: "civo", region: "fixture",
    region_code: "fixture", accepting: true,
    provision_token: PROVISION_TOKEN, registration_id: REGISTRATION,
  };
  const invite = { enabled: true, max_uses: 1, uses: 0,
    created_at: "2026-01-01T00:00:00.000Z", cell: CELL };
  const directory = new Map([
    [`cell:${CELL}`, JSON.stringify(cell)], [`invite:${B2.invite}`, JSON.stringify(invite)],
    ["config:placement", JSON.stringify({ strategy: "pinned", pinned_cell: CELL })],
  ]);
  const knownID = (id) => typeof id === "string" && (id === originalID || id === candidateID);
  const directoryKey = (key) => directory.has(key) || key === "config:reaper" ||
    key === `acct:${accountID}` || key === `pending:${accountID}`;
  const env = {
    CP_SIGNUP_LEGAL_ENFORCEMENT: "true", CP_SIGNUP_OPEN: "false",
    CP_SIGNUP_TURNSTILE_ENABLED: "true", CP_SIGNUP_TURNSTILE_SECRET_KEY: TURNSTILE_SECRET,
    CP_SIGNUP_DAILY_LIMIT_PER_IP: "1", CP_SIGNUP_DAILY_LIMIT_GLOBAL: "1",
    CP_ACCOUNT_BACKUPS_ENABLED: "false",
    PUBLIC_IP_LIMITER: { async limit(value) {
      check(value?.key === B2.sourceIP, "public limiter key"); counts.publicLimiter++;
      return { success: true };
    } },
    SIGNUP_IP_LIMITER: { async limit(value) {
      check(value?.key === B2.sourceIP, "signup limiter key"); counts.signupLimiter++;
      return { success: true };
    } },
    DIRECTORY: {
      async get(key, options) {
        check(directoryKey(key), "directory read key");
        check(options?.type === "json", "directory read type");
        const value = directory.get(key);
        return value === undefined ? null : JSON.parse(value);
      },
      async list({ prefix, cursor }) {
        check(prefix === "cell:" && cursor === undefined, "directory page");
        counts.placement++;
        return { keys: [{ name: `cell:${CELL}` }], list_complete: true, cursor: "" };
      },
      async put(key, text, options) {
        check(typeof text === "string" && text.length < 8192, "directory write bound");
        let value;
        try { value = JSON.parse(text); }
        catch { check(false, "invalid directory JSON"); }
        if (key === `invite:${B2.invite}`) {
          check(JSON.stringify(value) === JSON.stringify({ ...invite, uses: 1 }), "invite projection");
        } else if (key === `pending:${accountID}`) {
          check(value.cell === CELL && value.route_epoch === 0 && knownID(value.provision_id) &&
            Number.isFinite(Date.parse(value.created_at)), "pending projection");
          const keys = ["cell", "created_at", "route_epoch", "provision_id"];
          if (Object.hasOwn(value, "emails_sent")) {
            keys.push("emails_sent", "last_email_at");
            check(value.emails_sent === 1 && Number.isFinite(Date.parse(value.last_email_at)), "pending email projection");
          }
          exact(value, keys, "pending shape");
        } else if (key === `acct:${accountID}`) {
          exact(value, ["cell", "endpoint", "region", "region_code", "cell_registration_id", "epoch"], "route shape");
          check(value.cell === CELL && value.endpoint === cellOrigin && value.epoch === 0 &&
            value.cell_registration_id === REGISTRATION && value.region === cell.region &&
            value.region_code === cell.region_code, "route projection");
        } else if (/^verify:[0-9a-f]{64}$/.test(key)) {
          exact(value, ["account_id", "cell", "created_at"], "verification shape");
          check(value.account_id === accountID && value.cell === CELL &&
            Number.isFinite(Date.parse(value.created_at)) && options?.expirationTtl === 604800,
          "verification projection");
          verificationHashes.add(key.slice(7));
        } else check(false, "directory write key");
        if (!key.startsWith("verify:")) check(options === undefined, "unexpected directory TTL");
        directory.set(key, text);
      },
    },
    LEGAL_DOCUMENTS: { async fetch(request) {
      const url = urlOf(request);
      check(url.href === "https://legal.internal/legal/versions.json" && request.method === "GET" &&
        request.redirect === "error", "legal binding request");
      counts.canonical++;
      return authorityAvailable ? legal.fetch(request) : new Response(null, { status: 503 });
    } },
    EMAIL: { async send(value) {
      exact(value, ["to", "from", "subject", "text", "html"], "email shape");
      check(value.to === B2.email && value.from === "no-reply@witwave.ai" &&
        value.subject === "Verify your Witself account", "email addressing");
      const escapedOrigin = origin.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      const links = value.text.match(new RegExp(`${escapedOrigin}/verify/[0-9a-f]{64}`, "g")) ?? [];
      check(links.length === 1 && value.html.includes(links[0]), "verification link");
      const token = new URL(links[0]).pathname.slice("/verify/".length);
      check(verificationHashes.has(createHash("sha256").update(token).digest("hex")), "verification hash binding");
      counts.email++;
    } },
  };
  function storage(name) {
    check(roles.has(name), "signup storage role");
    if (!storages.has(name)) storages.set(name, new Storage(check, (key, value) => {
      if (key !== STATE_KEY) return;
      check(phaseEvents.length < 128, "phase trace bound");
      phaseEvents.push({ role: roles.get(name), phase: value.phase,
        registered: value.legal_successor?.registered ?? null });
      if (LEGAL_PHASES.has(value.phase) && !generatedLegalStates.has(value.phase)) {
        generatedLegalStates.set(value.phase, structuredClone(value));
      }
    }));
    return storages.get(name);
  }
  env.ACCOUNT_SIGNUP = {
    idFromName(name) { check(roles.has(name), "signup namespace identity"); return { name }; },
    get(id) {
      check(roles.has(id?.name), "signup namespace handle");
      return { async fetch(request) {
        const url = urlOf(request);
        const role = roles.get(id.name);
        const allowed = role === "invite" ? ["/invite/reserve"] :
          role === "ip" || role === "global" ? ["/counter/consume"] :
          role === "original" ? ["/run", "/legal/reconsent"] : ["/run", "/legal/reserve"];
        check(url.origin === "https://account-signup.internal" && request.method === "POST" &&
          allowed.includes(url.pathname), "signup role route");
        if (url.pathname === "/counter/consume") counts.counters++;
        if (url.pathname === "/invite/reserve") counts.invite++;
        if (url.pathname === "/legal/reserve") counts.reserve++;
        if (!objects.has(id.name)) objects.set(id.name,
          new AccountSignup({ id: { name: id.name }, storage: storage(id.name) }, env));
        return objects.get(id.name).fetch(request);
      } };
    },
  };
  env.CELL_COORDINATOR = {
    idFromName(name) { check(name === CELL, "target namespace identity"); return { name }; },
    get(id) {
      check(id?.name === CELL, "target namespace handle");
      return { async fetch(request) {
        const url = urlOf(request);
        check(url.origin === "https://target-cell.internal" && request.method === "POST" &&
          ["/provision/begin", "/provision/attach", "/provision/promote"].includes(url.pathname), "target route");
        const value = await jsonOf(request);
        check(value.cell_name === CELL && knownID(value.provision_id) &&
          value.registration_id === REGISTRATION, "target registration tuple");
        const base = ["cell_name", "provision_id", "registration_id"];
        if (url.pathname === "/provision/begin") {
          exact(value, base, "target begin shape");
          check(!targets.has(value.provision_id), "duplicate target begin");
          targets.set(value.provision_id, { attached: false, resident: false }); counts.targetBegin++;
          return Response.json({ ok: true, provision_id: value.provision_id, registration_id: REGISTRATION });
        }
        exact(value, [...base, "account_id", "route_epoch"], "target attachment shape");
        const target = targets.get(value.provision_id);
        check(target && value.account_id === accountID && value.route_epoch === 0, "target account tuple");
        if (url.pathname === "/provision/attach") {
          check(!target.attached, "duplicate target attach"); target.attached = true; counts.targetAttach++;
          return Response.json({ ok: true, provision_id: value.provision_id, account_id: accountID, attached: true });
        }
        check(target.attached && !target.resident, "target promotion order");
        target.resident = true; counts.targetPromote++;
        return Response.json({ ok: true, provision_id: value.provision_id, account_id: accountID, resident: true });
      } };
    },
  };

  async function dispatchFixtureFetch(input, init) {
    check(!closed, "closed fixture fetch");
    let request;
    try { request = new Request(input, init); }
    catch { check(false, "invalid fixture fetch request"); }
    const url = urlOf(request);
    if (url.href === "https://challenges.cloudflare.com/turnstile/v0/siteverify") {
      check(request.method === "POST", "Turnstile method");
      const form = new URLSearchParams(await request.text());
      check([...form].length === 3 && form.get("secret") === TURNSTILE_SECRET &&
        form.get("response") === B2.challenge && form.get("remoteip") === B2.sourceIP, "Turnstile form");
      counts.turnstile++;
      return Response.json({ success: true, "error-codes": [] });
    }
    check(url.origin === cellOrigin, "unexpected fetch origin");
    if (url.pathname === "/v1/version") {
      check(request.method === "GET", "cell protocol method"); counts.protocol++;
      return Response.json({ schema_version: "witself.v0", account_provision_protocol: 1 });
    }
    check(request.method === "POST" && request.headers.get("Authorization") === `Bearer ${PROVISION_TOKEN}`,
      "cell mutation authentication");
    const value = await jsonOf(request);
    if (url.pathname === `/v1/accounts/${accountID}:events`) {
      exact(value, ["verb", "actor_kind", "metadata"], "event shape");
      exact(value.metadata, ["to_masked"], "event metadata shape");
      check(value.verb === "account.email.verify.sent" && value.actor_kind === "control_plane" &&
        value.metadata.to_masked === "o***@e***.test", "verification event");
      counts.event++; return Response.json({ ok: true });
    }
    check(url.pathname === "/v1/accounts:provision-exact", "cell mutation route");
    exact(value, ["email", "display_name", "provision_id", "consent_terms_version", "consent_privacy_version"],
      "exact provision shape");
    check(knownID(value.provision_id) && value.email === B2.email && value.display_name === B2.displayName &&
      [currentPair, stalePair].some((pair) => value.consent_terms_version === pair.consent_terms_version &&
        value.consent_privacy_version === pair.consent_privacy_version), "exact provision core and pair");
    const prior = receipts.get(value.provision_id);
    if (prior) check(JSON.stringify(prior) === JSON.stringify(value), "immutable cell receipt");
    else {
      check(receipts.size === 0, "one logical account");
      receipts.set(value.provision_id, structuredClone(value));
    }
    counts.provision++; bootstrapSequence++;
    lastBootstrapToken = `b2-fake-bootstrap-${bootstrapSequence}`;
    return Response.json({
      schema_version: "witself.v0", provision_id: value.provision_id, replayed: !!prior,
      account: { account_id: accountID, operator_id: operatorID, email: B2.email,
        status: "pending", bootstrap_token: lastBootstrapToken },
      recorded_consent_terms_version: value.consent_terms_version,
      recorded_consent_privacy_version: value.consent_privacy_version,
    }, { status: 201 });
  }
  async function drain() {
    while (tasks.length) {
      const settled = await Promise.allSettled(tasks.splice(0));
      for (const result of settled) check(result.status === "fulfilled", "waitUntil rejected");
    }
  }
  function assertNoUnexpectedCalls() {
    assert.deepEqual(violations, [], "closed fixture boundary violations");
    assert.deepEqual(containerCalls, [], "signup must not fall through to containers");
  }
  const fixture = {
    currentPair, stalePair,
    async dispatchPublic(request) {
      check(!closed, "closed public fixture");
      const url = urlOf(request);
      const reconsent = originalID !== null && url.pathname === `/v1/account-signups/${originalID}:reconsent`;
      check(url.origin === origin && request.method === "POST" &&
        (url.pathname === "/v1/accounts" || reconsent), "public route");
      const raw = await request.clone().text();
      check(raw.length <= 65536, "public request bound");
      let body;
      try { body = JSON.parse(raw); }
      catch { check(false, "invalid public JSON"); }
      check(request.headers.get("CF-Connecting-IP") === B2.sourceIP &&
        body.email === B2.email && body.display_name === B2.displayName && body.invite === B2.invite,
      "public fixture core");
      if (cliMode) {
        const validID = (id) => typeof id === "string" && /^[A-Za-z0-9_-]{1,128}$/.test(id);
        if (!reconsent && originalID === null) {
          check(validID(body.provision_id), "CLI original identity");
          originalID = body.provision_id;
          roles.set(`provision:${originalID}`, "original");
        } else if (reconsent && candidateID === null) {
          const refused = storages.get(`provision:${originalID}`)?.values.get(STATE_KEY);
          check(refused?.phase === "legal_rejected" &&
            isDeepStrictEqual(body.refusal, refused.legal_refusal), "CLI exact predecessor refusal");
          check(validID(body.candidate?.provision_id) && body.candidate.provision_id !== originalID &&
            validID(body.transition_id), "CLI successor identities");
          candidateID = body.candidate.provision_id;
          transitionID = body.transition_id;
          roles.set(`provision:${candidateID}`, "candidate");
        }
      }
      if (reconsent) {
        check(body.refusal?.provision_id === originalID && body.candidate?.provision_id === candidateID &&
          body.transition_id === transitionID, "public successor identities");
        registrationHashes.push(createHash("sha256").update(raw).digest("hex")); counts.publicReconsent++;
      } else {
        check(knownID(body.provision_id), "public provision identity"); counts.publicSignup++;
      }
      const response = await worker.fetch(request, env, { waitUntil(promise) { tasks.push(promise); } });
      await drain(); assertNoUnexpectedCalls();
      if (reconsent && dropOuterAck) {
        check(response.status === 200, "outer ack loss requires completed handler");
        dropOuterAck = false; counts.droppedOuterAck++;
        await response.body?.cancel();
        throw new Error("B2 simulated lost outer registration acknowledgement");
      }
      return response;
    },
    dispatchFixtureFetch,
    setAuthorityAvailable(value) { check(typeof value === "boolean", "authority fault type"); authorityAvailable = value; },
    setEnforcement(value) { check(typeof value === "boolean", "enforcement fault type"); env.CP_SIGNUP_LEGAL_ENFORCEMENT = String(value); },
    loseNextOuterAcknowledgement() { check(!dropOuterAck, "one outer fault"); dropOuterAck = true; },
    restartSignupObjects() { objects.clear(); },
    snapshotRelationships() {
      const states = {};
      for (const [name, role] of roles) {
        if (role === "original" || role === "candidate") states[role] =
          structuredClone(storages.get(name)?.values.get(STATE_KEY) ?? null);
      }
      const allDurable = JSON.stringify([...storages.values()].map((value) => [...value.values]));
      for (const sensitive of [B2.email, B2.sourceIP, B2.challenge, TURNSTILE_SECRET,
        PROVISION_TOKEN, "b2-fake-bootstrap", '"bootstrap_token"', '"email"']) {
        check(!allDurable.includes(sensitive), "durable state privacy");
      }
      const counter = (name) => [...(storages.get(name)?.values ?? [])]
        .filter(([key]) => key.startsWith("count:")).map(([, value]) => value.count);
      return structuredClone({
        counts, states, phaseEvents, registrationHashes,
        counterCounts: { ip: counter(ipScope), global: counter("signup-counter:global") },
        inviteUses: JSON.parse(directory.get(`invite:${B2.invite}`)).uses,
        logicalAccounts: receipts.size, verificationEntries: verificationHashes.size,
        receiptPairs: [...receipts].map(([id, value]) => ({ role: id === originalID ? "original" : "candidate",
          consent_terms_version: value.consent_terms_version, consent_privacy_version: value.consent_privacy_version })),
        // At most three <=32-KiB synthetic records, for a separately pinned
        // historical-reader stage. No historical source is imported here.
        generatedLegalStates: Object.fromEntries(generatedLegalStates),
        ...(cliMode ? { cli: { originalID, candidateID, transitionID, ...cliCounts, currentPair } } : {}),
      });
    },
    assertNoUnexpectedCalls,
    async close() {
      if (closed) return;
      try { await drain(); assertNoUnexpectedCalls(); }
      finally { closed = true; globalThis.fetch = originalFetch; activeFixture = false; }
    },
  };
  if (cliMode) {
    fixture.dispatchCLILegalManifest = async (request, prior = false) => {
      const url = urlOf(request);
      check(!closed && request.method === "GET" && url.href === `${origin}/legal/versions.json` &&
        typeof prior === "boolean", "CLI manifest route");
      check(!prior || (cliCounts.manifest === 0 && originalID === null), "one initial manifest fault");
      cliCounts.manifest++;
      const response = legal.fetch(request);
      if (!prior) return response;
      const value = await response.json();
      value.terms.version = stalePair.consent_terms_version;
      value.privacy.version = stalePair.consent_privacy_version;
      return new Response(JSON.stringify(value), { status: response.status, headers: response.headers });
    };
    fixture.dispatchCLIBootstrap = async (request) => {
      const url = urlOf(request);
      check(!closed && request.method === "POST" && url.href === `${origin}/v1/auth/bootstrap`,
        "CLI bootstrap route");
      const value = await jsonOf(request);
      exact(value, ["bootstrap_token"], "CLI bootstrap body");
      check(lastBootstrapToken !== null && value.bootstrap_token === lastBootstrapToken &&
        !claimedBootstrap && receipts.size === 1 &&
        [...storages.values()].some((entry) => entry.values.get(STATE_KEY)?.phase === "completed"),
      "one exact completed-account bootstrap");
      claimedBootstrap = true;
      cliCounts.bootstrap++;
      return Response.json({ operator_id: operatorID, operator_token: "witself_opr_accountCreateRecovery" });
    };
  }
  resetContainerCalls(); activeFixture = true;
  // No network fallback: the saved function is only restored on close.
  globalThis.fetch = dispatchFixtureFetch;
  return fixture;
}
