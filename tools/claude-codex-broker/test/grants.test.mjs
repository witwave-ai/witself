import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import test from "node:test";

import { createGrant } from "../../../.claude/codex-profiles.mjs";
import { GrantVerifier, loadGrantAuthority } from "../lib/grants.mjs";
import { parseArgs } from "../lib/broker.mjs";
import { makeTemp } from "./helpers.mjs";

const NOW = 2_000_000_000_000;
const TASK = Object.freeze({ task: "Complete the bounded task." });
const JOB = Object.freeze({ job_id: "00000000-0000-4000-8000-000000000000" });

function grant(key, overrides = {}) {
  return createGrant({
    key,
    ceiling: overrides.ceiling ?? "system",
    toolName: overrides.toolName ?? "codex_system_start",
    mode: overrides.mode ?? "bypassPermissions",
    toolUseId: overrides.toolUseId ?? "toolu_test_123",
    sessionId: overrides.sessionId ?? "session-test",
    originalInput: overrides.originalInput ?? TASK,
    now: overrides.now ?? NOW,
    nonce: overrides.nonce ?? "abcdefghijklmnopQRSTUVWX",
  });
}

test("grant authenticates exact input and is atomically one use", () => {
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "system", key, clock: () => NOW + 100 });
  const proof = grant(key);
  assert.deepEqual(verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: proof }), TASK);
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: proof }), (error) => error?.code === "grant_replayed");
  const reusedToolUse = grant(key, { nonce: "uniquenonceABCDEFGH123456" });
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: reusedToolUse }), (error) => error?.code === "grant_replayed");
  const reusedNonce = grant(key, { toolUseId: "toolu_different" });
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: reusedNonce }), (error) => error?.code === "grant_replayed");
  const boundedSession = grant(key, {
    toolUseId: "toolu_bounded_session",
    sessionId: "Claude session / bounded workspace",
    nonce: "boundedsessionABCDEFGH123456",
  });
  assert.deepEqual(verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: boundedSession }), TASK);
  verifier.destroy();
});

test("grant rejects altered input, tool, ceiling, malformed keys, and bad MAC", () => {
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "system", key, clock: () => NOW });
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { task: "altered", _codex_grant: grant(key) }), (error) => error?.code === "grant_input_mismatch");
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: grant(key, { toolName: "codex_system_status" }) }), (error) => error?.code === "grant_invalid");
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: grant(key, { ceiling: "isolated-write" }) }), (error) => error?.code === "grant_invalid");
  const extra = { ...grant(key, { nonce: "differentnonceABCDEFGH1234" }), extra: true };
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: extra }), (error) => error?.code === "grant_invalid");
  const badMac = { ...grant(key, { nonce: "anothernonceABCDEFGH12345" }), mac: "A".repeat(43) };
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: badMac }), (error) => error?.code === "grant_authentication_failed");
  verifier.destroy();
});

test("grant enforces 30-second age, five-second future skew, and operation-specific modes", () => {
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "system", key, clock: () => NOW });
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: grant(key, { now: NOW - 30_001 }) }), (error) => error?.code === "grant_expired");
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: grant(key, { now: NOW + 5_001, nonce: "futurenonceABCDEFGH123456" }) }), (error) => error?.code === "grant_expired");
  assert.throws(() => verifier.verifyAndConsume("codex_system_start", { ...TASK, _codex_grant: grant(key, { mode: "plan", nonce: "planstartnonceABCDEFGH12" }) }), (error) => error?.code === "grant_invalid");
  const statusProof = grant(key, { toolName: "codex_system_status", mode: "plan", originalInput: JOB, nonce: "statusnonceABCDEFGH12345" });
  assert.deepEqual(verifier.verifyAndConsume("codex_system_status", { ...JOB, _codex_grant: statusProof }), JOB);
  verifier.destroy();
});

test("status grants bind the exact optional wait_seconds value", () => {
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "system", key, clock: () => NOW });
  const waitedStatus = { ...JOB, wait_seconds: 30 };
  const proof = grant(key, {
    toolName: "codex_system_status",
    mode: "plan",
    originalInput: waitedStatus,
    toolUseId: "toolu_wait_status",
    nonce: "waitstatusnonceABCDEFGH123",
  });
  assert.throws(() => verifier.verifyAndConsume("codex_system_status", {
    ...JOB, wait_seconds: 29, _codex_grant: proof,
  }), (error) => error?.code === "grant_input_mismatch");
  assert.deepEqual(verifier.verifyAndConsume("codex_system_status", {
    ...waitedStatus, _codex_grant: proof,
  }), waitedStatus);
  const malformedStatus = { ...JOB, wait_seconds: 31 };
  const malformedProof = grant(key, {
    toolName: "codex_system_status",
    mode: "default",
    originalInput: malformedStatus,
    toolUseId: "toolu_bad_wait_status",
    nonce: "badwaitstatusnonceABCDEFG12",
  });
  assert.throws(() => verifier.verifyAndConsume("codex_system_status", {
    ...malformedStatus, _codex_grant: malformedProof,
  }), (error) => error?.code === "grant_input_invalid");
  verifier.destroy();
});

test("isolated-write grants enforce implementation modes and exact bounded artifact inputs", () => {
  const key = crypto.randomBytes(32);
  const verifier = new GrantVerifier({ ceiling: "isolated-write", key, clock: () => NOW });
  const startProof = grant(key, {
    ceiling: "isolated-write", toolName: "codex_implementation_start", mode: "acceptEdits",
    toolUseId: "toolu_impl_start", nonce: "implementationstartnonce12",
  });
  assert.deepEqual(verifier.verifyAndConsume("codex_implementation_start", { ...TASK, _codex_grant: startProof }), TASK);
  const planStart = grant(key, {
    ceiling: "isolated-write", toolName: "codex_implementation_start", mode: "plan",
    toolUseId: "toolu_impl_plan", nonce: "implementationplannonce123",
  });
  assert.throws(() => verifier.verifyAndConsume("codex_implementation_start", { ...TASK, _codex_grant: planStart }), (error) => error?.code === "grant_invalid");
  const artifact = { job_id: JOB.job_id, artifact_id: "changes.patch", offset: 0, max_bytes: 49152 };
  const artifactProof = grant(key, {
    ceiling: "isolated-write", toolName: "codex_implementation_artifact_read", mode: "default",
    toolUseId: "toolu_impl_artifact", nonce: "implementationartifactnonce", originalInput: artifact,
  });
  assert.deepEqual(verifier.verifyAndConsume("codex_implementation_artifact_read", { ...artifact, _codex_grant: artifactProof }), artifact);
  const hostile = { ...artifact, artifact_id: "../../secret" };
  const hostileProof = grant(key, {
    ceiling: "isolated-write", toolName: "codex_implementation_artifact_read", mode: "default",
    toolUseId: "toolu_impl_hostile", nonce: "implementationhostilenonce1", originalInput: hostile,
  });
  assert.throws(() => verifier.verifyAndConsume("codex_implementation_artifact_read", { ...hostile, _codex_grant: hostileProof }), (error) => error?.code === "grant_input_invalid");
  verifier.destroy();
});

test("startup authority requires exact ceiling and canonical private key path equality", async (t) => {
  const root = await makeTemp("claude-codex-grant-");
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const keyFile = path.join(root, "grant.key");
  await fs.writeFile(keyFile, crypto.randomBytes(32), { mode: 0o600 });
  await fs.chmod(keyFile, 0o600);
  const environment = { WITSELF_CODEX_CEILING: "system", WITSELF_CODEX_GRANT_KEY_FILE: keyFile, SAFE: "preserved" };
  const authority = loadGrantAuthority({ ceiling: "system", grantKeyFile: keyFile, environment, clock: () => NOW });
  assert.equal(authority.verifier.ceiling, "system");
  assert.deepEqual(authority.launcherEnvironment, { SAFE: "preserved" });
  authority.verifier.destroy();
  assert.throws(() => loadGrantAuthority({ ceiling: "system", grantKeyFile: keyFile, environment: { ...environment, WITSELF_CODEX_CEILING: "isolated-write" } }), (error) => error?.code === "ceiling_mismatch");
  assert.throws(() => loadGrantAuthority({ ceiling: "system", grantKeyFile: keyFile, environment: { ...environment, WITSELF_CODEX_GRANT_KEY_FILE: `${keyFile}.other` } }), (error) => error?.code === "invalid_grant_configuration");
  assert.throws(() => loadGrantAuthority({ ceiling: "repository", grantKeyFile: keyFile, environment: {} }), (error) => error?.code === "invalid_grant_configuration");
});

test("startup argument parser freezes exact repository, ceiling, and grant-key syntax", () => {
  assert.deepEqual(parseArgs(["--repository", "/canonical/repo"]), { projectRoot: "/canonical/repo", ceiling: "repository", grantKeyFile: null });
  assert.deepEqual(parseArgs(["--repository", "/canonical/repo", "--ceiling", "system", "--grant-key-file", "/private/grant.key"]), {
    projectRoot: "/canonical/repo", ceiling: "system", grantKeyFile: "/private/grant.key",
  });
  assert.throws(() => parseArgs(["--repository", "/canonical/repo", "--ceiling", "system"]), (error) => error?.code === "usage");
  assert.throws(() => parseArgs(["--repository", "/canonical/repo", "--grant-key-file", "/private/key"]), (error) => error?.code === "usage");
  assert.throws(() => parseArgs(["--repository", "/canonical/repo", "--repository", "/other"]), (error) => error?.code === "usage");
});
