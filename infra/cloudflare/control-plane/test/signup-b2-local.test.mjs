// First B2 stage only: real public Worker and AccountSignup wrapper, closed
// local service fixtures. No CLI subprocess, old-reader or provider claim.
import assert from "node:assert/strict";
import test from "node:test";
import { B2, makeSignupB2Fixture } from "./fixtures/signup-b2-local.mjs";

const core = { email: B2.email, display_name: B2.displayName, invite: B2.invite };
function request(path, value) {
  return new Request(B2.origin + path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "CF-Connecting-IP": B2.sourceIP },
    body: JSON.stringify(value),
  });
}
function signup(id, pair) {
  return request("/v1/accounts", { ...core, provision_id: id,
    ...pair, turnstile_token: B2.challenge });
}
function transition(refusal, pair) {
  return { schema_version: "witself.signup-reconsent.v1", refusal,
    transition_id: B2.transition, candidate: { provision_id: B2.candidate, ...pair }, ...core };
}
function reconsent(value) {
  return request(`/v1/account-signups/${B2.provision}:reconsent`, value);
}
async function body(response, status) {
  assert.equal(response.status, status);
  assert.equal(response.headers.get("Cache-Control"), "private, no-store");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  assert.equal(response.headers.get("Referrer-Policy"), "no-referrer");
  assert.equal(response.headers.get("Strict-Transport-Security"), "max-age=31536000; includeSubDomains");
  return response.json();
}
async function complete(response, id, replayed = false) {
  const value = await body(response, 201);
  assert.deepEqual(Object.keys(value).sort(), ["schema_version", "provision_id", "replayed",
    "account_id", "operator_id", "email", "status", "verification_email_sent", "cell", "bootstrap_token"].sort());
  assert.deepEqual({ ...value, bootstrap_token: "omitted" }, {
    schema_version: "witself.v0", provision_id: id, replayed,
    account_id: "acct_b2_fixture", operator_id: "opr_b2_fixture", email: B2.email,
    status: "pending", verification_email_sent: true,
    cell: { name: "b2-cell", endpoint: B2.cellOrigin }, bootstrap_token: "omitted",
  });
  assert.match(value.bootstrap_token, /^b2-fake-bootstrap-[1-9][0-9]*$/);
  return value;
}
function assertCounts(fixture, expected) {
  const snapshot = fixture.snapshotRelationships();
  for (const [name, count] of Object.entries(expected)) assert.equal(snapshot.counts[name], count, name);
  assert.equal(snapshot.counts.publicLimiter, snapshot.counts.publicSignup + snapshot.counts.publicReconsent);
  assert.equal(snapshot.counts.signupLimiter, snapshot.counts.publicSignup + snapshot.counts.publicReconsent);
  fixture.assertNoUnexpectedCalls();
  return snapshot;
}
function oneAccount(fixture, role, pair, extra = {}) {
  const snapshot = assertCounts(fixture, {
    turnstile: 1, counters: 2, invite: 1, placement: 1, protocol: 1,
    targetBegin: 1, targetAttach: 1, targetPromote: 1, email: 1, event: 1,
    ...extra,
  });
  assert.deepEqual(snapshot.counterCounts, { ip: [1], global: [1] });
  assert.equal(snapshot.inviteUses, 1);
  assert.equal(snapshot.logicalAccounts, 1);
  assert.equal(snapshot.verificationEntries, 1);
  // The public signup response deliberately has no consent echo. The actual
  // runtime validates the cell's receipt; this records its exact accepted pair.
  assert.deepEqual(snapshot.receiptPairs, [{ role, ...pair }]);
  assert.equal(snapshot.states[role].phase, "completed");
  return snapshot;
}
async function stale(fixture) {
  const refusal = await body(await fixture.dispatchPublic(signup(B2.provision, fixture.stalePair)), 409);
  assert.deepEqual(Object.keys(refusal).sort(), ["schema_version", "code", "error", "provision_id",
    "request_fingerprint", "consent_terms_version", "consent_privacy_version", "refusal_id",
    "refusal_revision", "required_terms_version", "required_privacy_version"].sort());
  assert.equal(refusal.schema_version, "witself.signup-legal-refusal.v1");
  assert.equal(refusal.code, "signup_legal_stale");
  assert.equal(refusal.error, "signup legal acceptance is out of date");
  assert.equal(refusal.provision_id, B2.provision);
  assert.match(refusal.request_fingerprint, /^[0-9a-f]{64}$/);
  assert.match(refusal.refusal_id, /^[A-Za-z0-9_-]{1,128}$/);
  assert.ok(Number.isSafeInteger(refusal.refusal_revision) && refusal.refusal_revision > 0);
  assert.equal(refusal.consent_terms_version, fixture.stalePair.consent_terms_version);
  assert.equal(refusal.consent_privacy_version, fixture.stalePair.consent_privacy_version);
  assert.equal(refusal.required_terms_version, fixture.currentPair.consent_terms_version);
  assert.equal(refusal.required_privacy_version, fixture.currentPair.consent_privacy_version);
  const snapshot = assertCounts(fixture, {
    publicSignup: 1, publicReconsent: 0, canonical: 1, turnstile: 1, counters: 2,
    invite: 0, placement: 0, protocol: 0, provision: 0, reserve: 0,
    targetBegin: 0, targetAttach: 0, targetPromote: 0, email: 0, event: 0,
  });
  assert.equal(snapshot.states.original.phase, "legal_rejected");
  assert.deepEqual(snapshot.states.original.legal_refusal, refusal);
  assert.equal(snapshot.states.original.legal_successor, undefined);
  assert.equal(snapshot.states.candidate, null);
  assert.equal(snapshot.logicalAccounts, 0);
  assert.equal(snapshot.inviteUses, 0);
  assert.deepEqual(snapshot.counterCounts, { ip: [1], global: [1] });
  return refusal;
}
async function registered(fixture, value) {
  const ack = await body(await fixture.dispatchPublic(reconsent(value)), 200);
  assert.deepEqual(Object.keys(ack).sort(), ["schema_version", "status", "refusal", "transition_id",
    "candidate", "candidate_request_fingerprint"].sort());
  assert.deepEqual({ ...ack, candidate_request_fingerprint: "omitted" }, {
    schema_version: "witself.signup-reconsent-ack.v1", status: "registered",
    refusal: value.refusal, transition_id: value.transition_id, candidate: value.candidate,
    candidate_request_fingerprint: "omitted",
  });
  assert.match(ack.candidate_request_fingerprint, /^[0-9a-f]{64}$/);
  const snapshot = fixture.snapshotRelationships();
  assert.equal(snapshot.states.original.legal_successor.registered, true);
  assert.deepEqual(snapshot.states.original.legal_successor.candidate, value.candidate);
  assert.equal(snapshot.states.candidate.phase, "legal_reserved");
  assert.equal(snapshot.states.candidate.request_fingerprint, ack.candidate_request_fingerprint);
  assert.deepEqual(snapshot.states.candidate.legal_reservation.refusal, value.refusal);
  return ack;
}
async function fixtureFor(t) {
  const fixture = await makeSignupB2Fixture();
  t.after(() => fixture.close());
  return fixture;
}

test("B2 public wrapper current legal pair completes one exact invited account", async (t) => {
  const fixture = await fixtureFor(t);
  await complete(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), B2.provision);
  const snapshot = oneAccount(fixture, "original", fixture.currentPair,
    { canonical: 1, publicSignup: 1, publicReconsent: 0, provision: 1, reserve: 0 });
  const phases = snapshot.phaseEvents.filter((event) => event.role === "original").map((event) => event.phase);
  assert.deepEqual(phases.filter((phase, i) => i === 0 || phase !== phases[i - 1]), [
    "abuse_preflight", "legal_pending", "initialized", "invite_reserved", "cell_selected",
    "protocol_verified", "target_reserved", "cell_acknowledged", "target_attached",
    "pending_projected", "route_projected", "resident_promoted", "completed",
  ]);
});

test("B2 public wrapper stale refusal requires an explicit successor and inherits abuse receipts", async (t) => {
  const fixture = await fixtureFor(t);
  const refusal = await stale(fixture);
  // A Node caller that has not accepted stops here: no implicit registration
  // or client-behavior claim. The explicit next action selects this candidate.
  const value = transition(refusal, fixture.currentPair);
  await registered(fixture, value);
  const reserved = assertCounts(fixture, { canonical: 1, turnstile: 1, counters: 2,
    invite: 0, placement: 0, provision: 0, email: 0, reserve: 1 });
  assert.deepEqual(reserved.phaseEvents.slice(-3), [
    { role: "original", phase: "legal_rejected", registered: false },
    { role: "candidate", phase: "legal_reserved", registered: null },
    { role: "original", phase: "legal_rejected", registered: true },
  ]);
  await complete(await fixture.dispatchPublic(signup(B2.candidate, fixture.currentPair)), B2.candidate);
  oneAccount(fixture, "candidate", fixture.currentPair,
    { canonical: 2, publicSignup: 2, publicReconsent: 1, provision: 1, reserve: 1 });
});

test("B2 Node repeat after lost outer registration ack reuses one durable selection", async (t) => {
  const fixture = await fixtureFor(t);
  const value = transition(await stale(fixture), fixture.currentPair);
  fixture.loseNextOuterAcknowledgement();
  await assert.rejects(fixture.dispatchPublic(reconsent(value)),
    /B2 simulated lost outer registration acknowledgement/);
  const lost = fixture.snapshotRelationships();
  assert.equal(lost.states.original.legal_successor.registered, true);
  assert.equal(lost.states.candidate.phase, "legal_reserved");
  assert.equal(lost.logicalAccounts, 0);
  assert.equal(lost.counts.provision, 0);
  fixture.restartSignupObjects(); // Same Node process and retained Maps; no CLI restart claim.
  const ack = await registered(fixture, value);
  assert.equal(ack.candidate_request_fingerprint, lost.states.candidate.request_fingerprint);
  await complete(await fixture.dispatchPublic(signup(B2.candidate, fixture.currentPair)), B2.candidate);
  const snapshot = oneAccount(fixture, "candidate", fixture.currentPair,
    { canonical: 2, publicSignup: 2, publicReconsent: 2, reserve: 1, provision: 1, droppedOuterAck: 1 });
  assert.equal(snapshot.registrationHashes.length, 2);
  assert.equal(snapshot.registrationHashes[0], snapshot.registrationHashes[1]);
});

test("B2 authority failure preserves pending admission and cannot block an admitted exact retry", async (t) => {
  const fixture = await fixtureFor(t);
  fixture.setAuthorityAvailable(false);
  const unavailable = await body(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), 503);
  assert.equal(unavailable.error, "signup legal authority is unavailable");
  const pending = assertCounts(fixture, { canonical: 1, turnstile: 1, counters: 2,
    invite: 0, placement: 0, protocol: 0, provision: 0, email: 0 });
  assert.equal(pending.states.original.phase, "legal_pending");
  fixture.restartSignupObjects(); fixture.setAuthorityAvailable(true);
  const first = await complete(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), B2.provision);
  fixture.setAuthorityAvailable(false);
  const replay = await complete(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), B2.provision, true);
  assert.notEqual(replay.bootstrap_token, first.bootstrap_token);
  oneAccount(fixture, "original", fixture.currentPair,
    { canonical: 2, publicSignup: 3, publicReconsent: 0, provision: 2, reserve: 0 });
});

test("B2 local flag-off keeps legal checkpoints sticky while fresh invited signup skips legal checks", async (t) => {
  await t.test("pending remains authority-gated after local flag off", async (t) => {
    const fixture = await fixtureFor(t);
    fixture.setAuthorityAvailable(false);
    await body(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), 503);
    const before = fixture.snapshotRelationships().states.original;
    fixture.setEnforcement(false); fixture.restartSignupObjects();
    await body(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), 503);
    assert.deepEqual(fixture.snapshotRelationships().states.original, before);
    fixture.setAuthorityAvailable(true);
    await complete(await fixture.dispatchPublic(signup(B2.provision, fixture.currentPair)), B2.provision);
    oneAccount(fixture, "original", fixture.currentPair, { canonical: 3, provision: 1 });
  });
  await t.test("rejected replay and reserved successor survive local flag off", async (t) => {
    const fixture = await fixtureFor(t);
    const refusal = await stale(fixture);
    fixture.setEnforcement(false); fixture.setAuthorityAvailable(false); fixture.restartSignupObjects();
    assert.deepEqual(await body(await fixture.dispatchPublic(signup(B2.provision, fixture.stalePair)), 409), refusal);
    assert.equal(fixture.snapshotRelationships().counts.canonical, 1);
    const value = transition(refusal, fixture.currentPair);
    await registered(fixture, value);
    const reserved = fixture.snapshotRelationships().states.candidate;
    fixture.restartSignupObjects();
    await body(await fixture.dispatchPublic(signup(B2.candidate, fixture.currentPair)), 503);
    assert.deepEqual(fixture.snapshotRelationships().states.candidate, reserved);
    fixture.setAuthorityAvailable(true);
    await complete(await fixture.dispatchPublic(signup(B2.candidate, fixture.currentPair)), B2.candidate);
    const snapshot = oneAccount(fixture, "candidate", fixture.currentPair,
      { canonical: 3, reserve: 1, provision: 1 });
    assert.deepEqual(Object.keys(snapshot.generatedLegalStates).sort(),
      ["legal_pending", "legal_rejected", "legal_reserved"]);
    for (const [phase, state] of Object.entries(snapshot.generatedLegalStates)) {
      assert.equal(state.phase, phase);
      assert.ok(JSON.stringify(state).length <= 32768);
    }
    // Only generated, cloned synthetic snapshots are exposed. Actual v284
    // reader replay is a separate pinned acceptance stage, not a normal-test
    // dependency on historical Git objects or a copied legacy runtime.
    snapshot.generatedLegalStates.legal_pending.phase = "changed local copy";
    assert.equal(fixture.snapshotRelationships().generatedLegalStates.legal_pending.phase, "legal_pending");
  });
  await t.test("fresh false-flag invited signup performs no legal read", async (t) => {
    const fixture = await fixtureFor(t);
    fixture.setEnforcement(false); fixture.setAuthorityAvailable(false);
    await complete(await fixture.dispatchPublic(signup(B2.provision, fixture.stalePair)), B2.provision);
    const snapshot = oneAccount(fixture, "original", fixture.stalePair,
      { canonical: 0, publicSignup: 1, provision: 1 });
    assert.deepEqual(snapshot.generatedLegalStates, {});
  });
});
