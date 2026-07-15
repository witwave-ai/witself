# Witself Security Policy

Status: draft. This document describes the intended vulnerability reporting,
security response, and supported-surface policy before implementation.

Narrative-memory amendment (accepted 2026-07-14): memory inference and vector
generation are client-side. Security scope now includes curator credentials,
untrusted transcript evidence, fencing/plans, and client-supplied vectors; any
backend embedding-provider assumptions below are superseded by
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Witself spans two planes. The OPEN plane stores agent self/identity data
(memories, facts, policies, security groups, and inter-agent messages) and is
protected for its *integrity and authenticity*. The SEALED plane stores secret
material (secrets and TOTP enrollments) and is protected for its
*confidentiality*. Both postures are in scope, and the vulnerability classes
that matter most to Witself follow from that split; see
[Out Of Scope](#out-of-scope) and [threat-model.md](threat-model.md).

The sealed plane is held to its consolidation invariants: secret values and
TOTP seeds are never embedded, never returned by semantic recall, never in the
self-digest, never in a plaintext export, and never ingested from
`CLAUDE.md`/`AGENTS.md`. They are only ever returned through the reveal-gated,
audited value paths; see [secret-model.md](secret-model.md),
[encryption-model.md](encryption-model.md), and
[key-hierarchy.md](key-hierarchy.md).

## Reporting Vulnerabilities

Security issues should be reported privately before public disclosure.

Initial reporting channel:

- Email: `security@witwave.ai`

If that address changes, update this document and the repository-level
`SECURITY.md` before the first public release.

Reports should include:

- Affected component.
- Version, commit, or deployment mode if known.
- Steps to reproduce.
- Impact assessment.
- Whether the issue affects integrity or authenticity of open-plane identity
  data, for example cross-agent access bypass, policy misevaluation,
  security-group escalation, forged message senders, or memory poisoning.
- Whether the issue affects confidentiality of sealed-plane secret material, for
  example secret-value leakage, a reveal or authorization bypass, KMS or key
  mishandling, or unintended server-side decryption.
- Whether memory content, fact values, message bodies or payloads, PII,
  embedding vectors, secret values, TOTP seeds, generated TOTP codes, raw
  agent/operator tokens, payment data, or other customer data may have been
  exposed, altered, or destroyed.

Do not include real third-party credentials, payment details, wallet private
keys, production customer data, live identity content (real memories, facts, or
PII), or live secret material (secret values, TOTP seeds, or generated TOTP
codes) in a report unless a dedicated secure intake path exists.

## Expected Response

Initial target response expectations:

- Acknowledge credible reports within 3 business days.
- Triage severity and affected surfaces.
- Provide a remediation plan or status update when practical.
- Coordinate public disclosure after a fix or mitigation is available.

Integrity-impacting reports are prioritized: a confirmed cross-agent access
bypass, policy evaluation flaw, security-group escalation, message-sender
forgery, or memory-poisoning vector is treated as high severity by default
because it can corrupt or destroy identity data rather than merely expose it.

Confidentiality-impacting reports against the sealed plane are likewise treated
as high severity by default: a confirmed secret-value leak, reveal or
authorization bypass, KMS or key-handling flaw, or unintended server-side
decrypt can expose secret material that the envelope encryption and reveal
ceremony are meant to keep sealed.

These targets may change once Witself has a formal security operations process.

## Supported Surfaces

Security support applies to:

- Public `witself` CLI releases.
- Public `witself-server` releases once available.
- Published container images.
- Published Helm charts.
- Published Terraform modules and examples.
- Managed Witself Cloud.
- Production-supported self-hosted versions.

Development-only local mock mode is security-relevant, but it is not the
production security model. Issues in local mode should be reported when they can
affect production code paths, leak local identity data or local secret material,
weaken developer safety, weaken authorization or policy evaluation, or create
unsafe defaults.

The server's deterministic recall and optional client-vector boundary are
supported security surfaces. Scope/profile/version/content-hash validation
bypasses, non-finite or dimension-confused vectors, cross-owner vector writes,
unsafe vector logging, and ranking-integrity failures are in scope. The backend
does not call an embedding provider or possess model credentials; client model
security remains outside the server boundary except where submitted data
crosses the Witself API. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

The KMS-provider abstraction (`aws-kms`, `gcp-kms`, `azure-key-vault`,
`local-dev`) is the sealed plane's required dependency when that plane is
enabled. Issues that let secret material be decrypted, exported, or logged in
plaintext outside the reveal path, that mishandle the CMK / per-realm KEK /
per-secret-or-field DEK envelope, or that cause unintended server-side decrypt,
are in scope; see [key-hierarchy.md](key-hierarchy.md) and
[encryption-model.md](encryption-model.md).

## Sensitive Data Handling

Security reports, logs, crash dumps, support tickets, and diagnostics must not
include:

- Memory content.
- Fact values, including `sensitive` facts and PII.
- Message subjects, bodies, or structured payloads.
- Embedding vectors.
- Secret values.
- TOTP seeds.
- Generated TOTP codes.
- Raw agent/operator tokens.
- Self-hosted database URLs or storage credentials.
- Client-side model credentials captured by optional client tooling. They are
  never backend configuration or server-held secrets.
- KMS-provider credentials, key identifiers, or wrapped key material.
- Raw payment details.
- Wallet seed phrases, private keys, or raw wallet credentials.
- Provider credentials.

If a report requires evidence involving identity content, secret material, or
other sensitive data, the report should use redacted values or test identity
data and test credentials created only for reproduction.

The two planes carry different headline risks. For the open plane (memories,
facts, PII) the risk is corruption or unauthorized mutation as much as
exposure. For the sealed plane (secrets, TOTP) the risk is exposure of values
that should only ever leave through the reveal-gated, audited path: secret
values, TOTP seeds, and generated TOTP codes are never embedded, recalled, put
in the self-digest, or plaintext-exported, so any appearance of them outside a
reveal is itself a finding.

## In-Scope Vulnerability Classes

The following classes are explicitly in scope and are the kinds of issues
Witself most wants reported. The open-plane classes follow the
integrity-and-authenticity threat framing in
[threat-model.md](threat-model.md) and the authorization model in
[access-policy.md](access-policy.md). The sealed-plane classes follow the
confidentiality threat framing in [threat-model.md](threat-model.md), the
authorization model in
[authorization-and-roles.md](authorization-and-roles.md), and the encryption
model in [encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md).

Cross-agent access bypass:

- Any path that lets a token-bound agent read, recall, contribute to, curate, or
  forget another agent's (or a group's) memories or facts without a matching
  `allow` policy or operator authorization.
- Default-deny failures, where absence of a matching policy does not deny.
- Identity-reference (`witself://agent/...`, `witself://group/...`) resolution
  that returns cross-agent or cross-group data without re-checking authorization
  at resolve time.
- Scope-constraint bypass, where a scope limited by realm, owning agent, group,
  memory kind/tag, or fact name is not actually enforced.

Policy evaluation flaws:

- Incorrect evaluation of subject × permission × target × scope that grants
  access the policy author did not intend.
- Permission-verb confusion, where `read` is treated as `contribute`/`curate`/
  `forget`, or where an escalating verb is granted by a lesser one.
- `policy test` returning a decision that disagrees with the decision actually
  enforced on the live read/write path.
- Filter bypass (kind/tag/name/`sensitive`) that widens a policy beyond its
  stated filter.
- Operator-override paths that skip audit, `--reason`, or confirmation
  requirements on destructive or cross-agent actions.

Security-group escalation:

- Membership changes that grant an agent group access without `group:manage` or
  operator authorization.
- An agent gaining a group's policy-subject permissions without being an
  enforced member, or retaining them after removal.
- Group-scoped (collective) memories or facts becoming readable or writable to
  non-members, or to members beyond the group's bound policy.
- Group-admin/owner escalation that lets a member self-promote to manage
  membership or policies.

Message spoofing / forged sender:

- Any path where a message's `from` is not derived from the authenticated token,
  letting a caller send as another agent.
- Recipient (`to`) tampering, mis-fan-out to non-members of a target group, or
  delivery to an unintended mailbox.
- Forged or replayed delivery/read/ack state, or thread/conversation injection
  that misattributes ordering or authorship.
- Bypass of `message:send`/`message:read` scopes or of send/delivery rate
  limits.

Memory poisoning:

- Writing or curating memories/facts as or into another agent without policy,
  including via message-driven writes (a received message must not itself
  authorize a cross-agent write).
- Tampering with client-supplied vectors, vector profiles, FTS documents,
  salience, links, tags, kind, or `source` so recall surfaces
  attacker-controlled or misattributed content.
- Forging or corrupting versioned edit history, or making a `forget`/`delete`
  evade tombstoning, the retention window, or audit attribution.
- Poisoning import/restore or group-shared identity data so trusted recall
  returns falsified facts or memories.

The remaining classes protect the *confidentiality* of the sealed plane
(secrets and TOTP enrollments).

Secret leakage:

- Any path that exposes a secret value, TOTP seed, or generated TOTP code
  outside the reveal-gated, audited value paths (`secret reveal`, `totp code`,
  value-returning `reference resolve`).
- Secret material appearing in logs, traces, crash dumps, metrics, the audit
  log, error messages, or API responses that should carry only ciphertext or
  metadata.
- Secret values or TOTP seeds being embedded, returned by semantic recall,
  placed in the self-digest, included in a plaintext export, or ingested from
  `CLAUDE.md`/`AGENTS.md` — all of which the sealed plane prohibits; see
  [secret-model.md](secret-model.md).
- A plaintext export, digest emit, or ingest that includes the sealed plane
  rather than restricting secret backup to encrypted-only blobs; see
  [backup-and-recovery.md](backup-and-recovery.md).

Reveal / authorization bypass:

- Returning a secret value or TOTP code without the `secret:reveal` /
  `totp:code` scope, the reveal ceremony, or audit attribution.
- Secret references (`witself://secret/...`, `witself://agent/<agent>/secret/...`,
  `witself://group/<group>/secret/...`) resolving to a value without
  re-checking authorization, ownership, or an active grant at resolve time.
- A grant being honored beyond its bound owner agent or group, after revocation,
  or with broader scope than `secret:grant` issued; or a realm role
  (`realm:operator`, `realm:auditor`) reaching secret values it should not.
- Scope-constraint bypass where a scope limited by realm, owning agent, group,
  or secret path is not actually enforced on a value-returning path.

KMS / key handling:

- Mishandling of the CMK → per-realm KEK → per-secret-or-field DEK envelope:
  reused, predictable, cross-realm, or cross-secret DEKs, or a missing AEAD
  integrity check (`XCHACHA20_POLY1305`, `AES_256_GCM`); see
  [key-hierarchy.md](key-hierarchy.md).
- DEKs, KEKs, or KMS credentials persisted, cached, logged, or exported in
  plaintext, or key material crossing a realm boundary.
- KEK rotation that leaves prior DEKs decryptable when they should be retired,
  or that breaks the crypto-shred property on KMS-key destruction.
- KMS-provider misconfiguration (`aws-kms`, `gcp-kms`, `azure-key-vault`,
  `local-dev`) that silently weakens or disables envelope encryption rather
  than failing closed.

Server-side decrypt:

- The `server_side_decrypt` capability decrypting secret material when
  `client_side_decrypt` was the contracted posture, or expanding the trusted
  computing base beyond what the capability switch authorizes; see
  [encryption-model.md](encryption-model.md).
- Plaintext secret values lingering server-side after a server-mediated reveal,
  in memory, temp storage, or response buffers, beyond the reveal that
  authorized them.
- A reveal flowing through server-side decrypt without the `server_side_decrypt`
  flag being set on the audit event; see [audit-retention.md](audit-retention.md).

The following classes cover cross-realm collaboration, which is post-v0:
realm-local inter-agent messaging ships in v0, but cross-realm federation is the
first post-v0 epic, so these classes become in scope as that surface lands; see
[agent-collaboration.md](agent-collaboration.md). They guard the cross-realm
identity root (the signed realm card, federation allow-list, and blind relay)
and the loop/budget safety limits.

Realm-card forgery / signature bypass (post-v0):

- A forged or unsigned realm card being accepted in place of the mandatory-JWS
  card served at `/.well-known/witself-card.json`, or a card whose JWS is not
  verified against a trusted realm key.
- Signature-verification bypass against the realm JWKS published in the card, so
  that a signed envelope from another realm is accepted without a valid
  signature over a trusted published key.
- Any path that lets the federation card private key or the realm/agent signing
  keys (the cross-realm identity root) be committed, exported, logged, or
  otherwise leave the realm.

Federation allow-list bypass (post-v0):

- A peer realm being treated as allowed without a matching entry in the
  deny-by-default allow-list / trust registry, or absence of an entry failing to
  deny.
- Allow-list or realm-card publishing/rotation changes made without the
  `federation:manage` operator scope.
- Forged or spoofed cross-realm addressing (`witself://<realm-handle>/agent/<name>`)
  or `thr_` conversation injection that misattributes a cross-realm
  conversation's authorship or ordering.

Blind-relay body inspection (post-v0):

- The blind relay inspecting, logging, altering, or retaining cross-realm
  envelope bodies rather than forwarding signed envelopes opaquely, or the
  global directory / control plane carrying anything beyond routing metadata.

Loop / budget kill-switch evasion (post-v0):

- A cross-realm conversation evading the loop and budget safety limits
  (`max_hops=8`, `turn_budget=24`, TTL 1h, $5 soft / $25 hard kill-switch), or
  suppressing the `budget.exhausted` / `loop.suspended` audit events that should
  fire when they trip.

In all cases, the strongest reports demonstrate a concrete authorization,
integrity, authenticity, or confidentiality violation against a supported
surface, not a theoretical one.

## Disclosure Policy

Before public disclosure, Witself maintainers should:

- Confirm affected versions or deployment modes.
- Decide whether managed Witself Cloud, self-hosted deployments, local mode,
  MCP, CLI, Helm, Terraform, or release artifacts are affected.
- Assess identity-data impact: whether memories, facts, policies, groups,
  messages, or audit records may have been read, altered, destroyed, or
  misattributed, and whether affected realms must re-verify identity state.
- Assess secret-confidentiality impact: whether secret values or TOTP seeds may
  have been exposed, and whether affected realms must rotate exposed secrets,
  re-enroll TOTP, or rotate the per-realm KEK; see
  [key-hierarchy.md](key-hierarchy.md).
- Patch or mitigate the issue.
- Publish fixed releases or operational mitigations.
- Update docs when user action is required.

Security advisories should avoid publishing exploit details that materially
increase user risk before users have had time to update. For integrity issues,
advisories should also tell operators how to detect and recover affected
identity data, for example reviewing audit records, restoring forgotten
memories within the retention window, or re-importing from a trusted export; see
[backup-and-recovery.md](backup-and-recovery.md).

## Security Release Requirements

Security fixes should preserve the release hardening requirements:

- Signed release archives.
- Signed checksum manifests.
- Signed container images.
- SBOMs where available.
- Provenance or attestations where available.
- Public release notes with appropriate redaction.

When a vulnerability affects a Helm chart or Terraform module, release notes
should clearly identify whether users need to update application images, upgrade
charts, apply Terraform changes, or change storage, client-vector, KMS, or
deployment configuration.

When a vulnerability affected authorization, policy evaluation, group
membership, messaging, or identity integrity, release notes should also state
whether operators should audit recent cross-agent activity, review policy and
group state, or restore affected memories or facts.

When a vulnerability affected secret confidentiality, reveal authorization, key
handling, or server-side decrypt, release notes should also state whether
operators should rotate exposed secrets, re-enroll TOTP, rotate the per-realm
KEK, review reveal and grant audit records, or change KMS configuration.

## Out Of Scope

The following are generally out of scope unless they reveal a concrete Witself
security issue:

- Reports against unsupported development snapshots without reproduction on a
  supported release.
- Generic dependency scanner output without an exploitable Witself path.
- Social engineering against maintainers or users.
- Physical attacks against user devices.
- Actions taken by an agent or operator who already holds the authorization the
  action requires (for example an agent reading its own memories, or a policy
  grantee exercising the access a policy intentionally allows).
- "Poisoned" content in memories or messages that a receiving agent or runtime
  chose to trust without applying its own validation, where Witself correctly
  attributed and authorized the write. Message bodies and payloads are untrusted
  input to the receiving agent by design; see
  [inter-agent-messaging.md](inter-agent-messaging.md).
- Plaintext identity export performed by an authorized caller, since first-class
  plaintext export of the open plane is an intended Witself feature, not a leak;
  see [backup-and-recovery.md](backup-and-recovery.md). This carve-out does not
  extend to the sealed plane: a plaintext export that included secret values
  would itself be a finding.
- Attacks requiring already-authorized access to a secret value after it has
  been intentionally revealed and exported outside Witself, since Witself no
  longer controls material a caller chose to take out of the sealed plane.

## Related Docs

- [threat-model.md](threat-model.md)
- [access-policy.md](access-policy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [audit-retention.md](audit-retention.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [governance-and-support.md](governance-and-support.md)
- [release-and-build.md](release-and-build.md)
- [self-hosting.md](self-hosting.md)
- [api-contract.md](api-contract.md)
