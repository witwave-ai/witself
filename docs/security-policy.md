# Witself Security Policy

Status: draft. This document describes the intended vulnerability reporting,
security response, and supported-surface policy before implementation.

Witself stores agent self/identity data (memories, facts, policies, security
groups, and inter-agent messages). Where Witpass protects the *confidentiality*
of secret material, Witself protects the *integrity and authenticity* of
identity data. The vulnerability classes that matter most to Witself follow from
that flip; see [Out Of Scope](#out-of-scope) and
[threat-model.md](threat-model.md).

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
- Whether the issue affects integrity or authenticity of identity data, for
  example cross-agent access bypass, policy misevaluation, security-group
  escalation, forged message senders, or memory poisoning.
- Whether memory content, fact values, message bodies or payloads, PII,
  embedding vectors, raw agent/operator tokens, payment data, or other customer
  data may have been exposed, altered, or destroyed.

Do not include real third-party credentials, payment details, wallet private
keys, production customer data, or live identity content (real memories, facts,
or PII) in a report unless a dedicated secure intake path exists.

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
affect production code paths, leak local identity data, weaken developer safety,
weaken authorization or policy evaluation, or create unsafe defaults.

The embedding-provider abstraction (`voyage`, `openai`, `local-dev`) is a
configurable production dependency. Issues that let identity content leak to or
through an embedding provider, or that let a degraded provider silently weaken
authorization or recall correctness, are in scope; see
[memory-model.md](memory-model.md).

## Sensitive Data Handling

Security reports, logs, crash dumps, support tickets, and diagnostics must not
include:

- Memory content.
- Fact values, including `sensitive` facts and PII.
- Message subjects, bodies, or structured payloads.
- Embedding vectors.
- Raw agent/operator tokens.
- Self-hosted database URLs or storage credentials.
- Embedding-provider credentials.
- Raw payment details.
- Wallet seed phrases, private keys, or raw wallet credentials.
- Provider credentials.

If a report requires evidence involving identity content or other sensitive
material, the report should use redacted values or test identity data created
only for reproduction.

Note the domain difference from Witpass: there is no reveal ceremony, TOTP
seed, generated TOTP code, or local vault passphrase to leak. The protected
payload is identity data and PII, and the headline risk is its corruption or
unauthorized mutation, not only its exposure.

## In-Scope Vulnerability Classes

The following classes are explicitly in scope and are the kinds of issues
Witself most wants reported. They follow the integrity-and-authenticity threat
framing in [threat-model.md](threat-model.md) and the authorization model in
[access-policy.md](access-policy.md).

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
- Tampering with embeddings, salience, links, tags, kind, or `source` so that
  semantic recall surfaces attacker-controlled or misattributed content.
- Forging or corrupting versioned edit history, or making a `forget`/`delete`
  evade tombstoning, the retention window, or audit attribution.
- Poisoning import/restore or group-shared identity data so trusted recall
  returns falsified facts or memories.

In all cases, the strongest reports demonstrate a concrete authorization,
integrity, or authenticity violation against a supported surface, not a
theoretical one.

## Disclosure Policy

Before public disclosure, Witself maintainers should:

- Confirm affected versions or deployment modes.
- Decide whether managed Witself Cloud, self-hosted deployments, local mode,
  MCP, CLI, Helm, Terraform, or release artifacts are affected.
- Assess identity-data impact: whether memories, facts, policies, groups,
  messages, or audit records may have been read, altered, destroyed, or
  misattributed, and whether affected realms must re-verify identity state.
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
charts, apply Terraform changes, or change storage, embedding-provider, or
deployment configuration.

When a vulnerability affected authorization, policy evaluation, group
membership, messaging, or identity integrity, release notes should also state
whether operators should audit recent cross-agent activity, review policy and
group state, or restore affected memories or facts.

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
  plaintext export is an intended Witself feature, not a leak; see
  [backup-and-recovery.md](backup-and-recovery.md).

## Related Docs

- [threat-model.md](threat-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [governance-and-support.md](governance-and-support.md)
- [release-and-build.md](release-and-build.md)
- [self-hosting.md](self-hosting.md)
- [api-contract.md](api-contract.md)
