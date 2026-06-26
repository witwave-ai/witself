# Witself Post-v0 Roadmap

Status: draft. This document records features that are intentionally deferred
from v0. They are product candidates, not first-release blockers.

## Decision

V0 should stay focused on the core agent self/identity store:

- Installable `witself` CLI.
- MCP stdio support.
- Managed and self-hosted backend API shape.
- Named realms and named agents.
- Agent tokens bound to one realm and one named agent.
- Memory CRUD with the add/adjust/read/recall/list/forget/restore/delete
  lifecycle, versioned edit history, and soft-delete tombstones.
- Semantic-by-default recall over an embedding-provider abstraction (`voyage`
  default, `openai`, `local-dev`) with hybrid keyword/tag/kind/time ranking and
  deterministic degradation when the provider is unavailable.
- Facts CRUD with deterministic name lookup, `primary` promotion, and the
  `sensitive` PII display flag.
- The cross-agent access policy engine with default deny, the
  `read`/`contribute`/`curate`/`forget` verbs, and `policy test`.
- Security groups as policy subject and target, including group-scoped shared
  memories and facts.
- Inter-agent messaging in full: durable mailbox, delivery, ordering, and
  acknowledgement.
- First-class structured/plaintext identity export and round-trippable import of
  the open plane (memories and facts); secrets are never in the plaintext export.
- The sealed credential plane as a defined v0 slice that may be staged after the
  open-plane core: secret CRUD with redaction and explicit reveal, password
  generation, authenticator-app TOTP enrollment and code generation, runtime
  secret references and injection, per-realm KEK envelope encryption, and secret
  grants/realm roles (see [secret-model.md](secret-model.md),
  [totp-2fa.md](totp-2fa.md), [encryption-model.md](encryption-model.md), and
  [key-hierarchy.md](key-hierarchy.md)).
- Audit events.
- JSON, API, and MCP contracts.
- Public images, Helm charts under `charts/*`, Terraform, CI, linting, and
  release automation.

The features below should be documented now and revisited after v0 hardening.
They are also previewed in [requirements.md](requirements.md). Items that touch
the sealed credential plane preserve the sealed-plane carve-outs: secret values
and TOTP seeds are never embedded, recalled, placed in the self-digest, or
included in any plaintext export, and they remain reveal-gated.

## Deferred Recall And Memory Intelligence

### Richer Recall Ranking

Reranker models, learned recall weights, and feedback-driven ranking are
post-v0. V0 ships a fixed, documented hybrid blend of vector similarity,
lexical match, tag/kind match, recency, and `salience`
(see [memory-model.md](memory-model.md)).

A second-stage reranker (cross-encoder or hosted rerank API) changes the
provider boundary, adds a new metered dimension and capability flag, and needs a
deterministic answer for when the reranker is unavailable. It should follow the
same degrade-and-report contract that semantic recall already uses.

### Multi-Vector And Chunked Embeddings

Per-memory multi-vector embeddings, content chunking, and per-chunk recall are
post-v0. V0 embeds one vector per memory from its `content`.

Multi-vector storage changes the pgvector schema, the vector-storage metering
dimension, the re-embedding maintenance path, and recall result attribution
(which chunk matched). It requires migration and backup notes before promotion
because restored vectors must round-trip.

### Summarization And Consolidation

Automatic memory summarization, consolidation, and decay (merging related
memories, distilling episodic into semantic, pruning stale low-salience
memories) are post-v0.

Consolidation is an integrity-sensitive write path: it edits or forgets memories
the agent did not explicitly touch. It needs a written threat-model update for
silent memory mutation, audit attribution distinct from agent and operator
actions, `--dry-run` previews, reversible tombstones, and an opt-in capability
flag. It must not run as an automatic side effect in v0.

### Automated Re-Embedding And Model Migration

Automated re-embedding and embedding-model migration tooling are post-v0. V0
treats re-embedding on a provider/model change as an explicit, audited
maintenance operation, and restores recall from backed-up vectors rather than
re-embedding (see [backup-and-recovery.md](backup-and-recovery.md)).

A managed migration tool needs progress tracking, idempotency, cost metering,
and a rollback story for partially re-embedded realms.

## Deferred Cross-Realm Federation

### Cross-Realm And Cross-Account Federation

Cross-realm and cross-account federation of identity and policy is post-v0. V0
scopes agents, memories, facts, policies, groups, and messages to a single realm
(see [access-policy.md](access-policy.md) and
[security-groups.md](security-groups.md)).

Federation needs a trust model between realms, a federated identity-reference
form beyond today's `witself://` scheme, cross-realm policy evaluation, audit
that attributes the originating realm, and per-realm metering for federated
accesses. It is a large surface and must not be implied by single-realm v0
contracts.

## Deferred Messaging Transport

### Real-Time And Streaming Messaging

Real-time delivery, streaming, push, and subscription-based message transport
are post-v0. V0 inter-agent messaging is a durable mailbox/queue with poll-based
`message list`/`message read`, at-least-once delivery, per-recipient ordering,
and explicit acknowledgement (see
[inter-agent-messaging.md](inter-agent-messaging.md)).

A streaming transport (server-sent events, websockets, or long-poll) adds a new
network surface, connection authentication, backpressure and fan-out rules for
group delivery, and a higher-risk remote tool boundary. It should be reviewed
alongside MCP network transport.

### Message Attachments And Large Payloads

Message attachments and large structured payloads in object/blob storage are
post-v0. V0 messages carry a free-form `body` and an optional inline structured
`payload`.

Attachment storage needs size limits, a blob-adapter path, redaction rules for
diagnostics, and metering for stored attachment bytes. Because message content
is untrusted input to the receiving agent, attachment handling also needs an
explicit injection and memory-poisoning review before promotion.

## Deferred Sealed-Plane 2FA Modalities

These extend the sealed credential plane. V0 ships authenticator-app style TOTP
first (see [totp-2fa.md](totp-2fa.md)). All of the modalities below keep the
sealed-plane carve-outs: any seed or credential material they introduce is
KMS-backed sealed material, is never embedded, recalled, placed in the
self-digest, or plaintext-exported, and is reveal-gated.

### SMS And Email-Code 2FA

SMS and email-code capture are post-v0. They require integrations with inboxes,
phone-number providers, anti-abuse controls, privacy rules, retry behavior,
delivery failure handling, and support boundaries.

V0 solves authenticator-app style TOTP first
(see [totp-2fa.md](totp-2fa.md)).

### Push Approvals

Push approval flows are post-v0. They need a device/app approval channel,
human-in-the-loop policy, timeout behavior, audit semantics, and a clear answer
for unattended agents. Because an approval can gate a `secret reveal` or `totp
code`, the reveal ceremony and its audit attribution must compose with the
approval channel rather than bypass it.

### Passkeys

Passkey support is post-v0. It needs a WebAuthn/passkey agent story, origin and
browser binding decisions, recovery behavior, and a clear security model for
agents using credentials that are normally tied to a user device. Any stored
passkey material is sealed-plane material under the per-realm KEK envelope
(see [key-hierarchy.md](key-hierarchy.md)).

### Hardware Security Keys

Hardware security key support is post-v0. It requires physical device access,
handoff rules, unattended-agent policy, operator approval flows, and clear
deployment constraints for containers and Kubernetes workloads.

## Deferred Agent Login Helpers

### Browser-Session Handoff

Browser-session handoff is post-v0. It could eventually help agents complete
browser logins without exposing credentials broadly, but it changes the trust
model around browser automation, session cookies, server-side decrypt, runtime
injection, and support diagnostics.

V0 provides secrets, TOTP codes, secret references, and runtime injection through
`witself run` (see [secret-model.md](secret-model.md) and
[cli-command-surface.md](cli-command-surface.md)). It does not try to own the
browser session lifecycle. Any handoff must keep secret values reveal-gated and
out of recall, the self-digest, and any plaintext export.

## Deferred Admin And Access Surfaces

### Web Dashboard

A web dashboard is post-v0 and optional. Witself should continue to treat the
CLI as the primary control plane for account, realm, agent, memory, fact,
policy, group, message, billing, payment, usage, and support workflows
(see [requirements.md](requirements.md)).

A dashboard may exist later for convenience, but managed-service administration
must not require it, and it must reuse the same public API, authorization,
audit, and redaction rules as the CLI rather than adding a privileged web-only
path.

### Private Witself Admin CLI

A private internal Witself admin CLI is post-v0. It should be a separate tool
for Witself staff and trusted internal AI support/admin agents, not part of the
public customer/operator `witself` CLI.

The public CLI remains the customer-facing control plane.

## Deferred MCP Transport

### MCP HTTP And Network Transport

MCP HTTP or other network transport is post-v0. The default v0 MCP transport is
local stdio (see [mcp-tools.md](mcp-tools.md)).

Network MCP transport must be explicitly enabled, authenticated, scoped,
rate-limited where appropriate, and reviewed as a higher-risk remote tool
surface before it is promoted. Because MCP tools can drive cross-agent and
messaging writes, network transport must enforce the same policy engine and
audit attribution as the CLI and API.

## Deferred Provider And Cloud Targets

### Additional Embedding Providers

Additional embedding providers and on-cluster local embedding services are
post-v0. V0 ships the `voyage` (default), `openai`, and `local-dev` providers
behind the capability-gated embedding boundary
(see [storage.md](storage.md) and [memory-model.md](memory-model.md)).

New providers must report model identity and vector dimensionality through the
capabilities contract, preserve the degrade-to-keyword behavior, and document
the re-embedding impact of switching providers. On-cluster embedding services
additionally need deployment, scaling, and observability guidance for
self-hosted operators.

### GCP And Azure General Availability

GCP and Azure general availability is post-v0. V0 implements AWS first for
managed Witself Cloud, the first self-hosted Terraform module and stack, the
first production Postgres/pgvector (RDS) integration, and the first
production-shaped Helm values (see [cloud-targets.md](cloud-targets.md)).

GCP and Azure keep visible repo structure and provider-neutral interfaces so
AWS-only assumptions do not leak, but their full Terraform modules, managed
Postgres/pgvector integrations, object/blob storage, workload identity, and
production Helm examples follow AWS. Promotion to GA requires the same
backup/restore, migration, upgrade, and observability guidance demanded of AWS.

## Deferred Policy And Encryption Surfaces

### Richer Policy Expressions

Policy `deny` effects and richer policy expressions beyond the v0
default-deny/allow model are post-v0. V0 grants are `allow`-only, and the
absence of a matching allow is a deny (see [access-policy.md](access-policy.md)).

Explicit `deny`, precedence rules, condition expressions, and time-bounded
grants change policy evaluation semantics and `policy test` output. They need a
threat-model update for evaluation ambiguity and clear, deterministic conflict
resolution before promotion.

### Field-Level Encryption As A Managed Default

Field-level encryption of `sensitive` facts as a managed default is post-v0. V0
treats data-at-rest encryption (managed RDS/disk) as the baseline and offers
field-level encryption of `sensitive` facts as an optional capability, not the
default (see [storage.md](storage.md)).

Promoting it to a managed default reintroduces key-management operations, a
backup/restore key-availability requirement, and recovery edge cases. It must be
designed without resurrecting a secret-style reveal ceremony; `sensitive`
remains a PII/redaction flag, not an encryption boundary. A credential belongs
in the sealed plane (a secret), not in a `sensitive` fact
(see [facts-model.md](facts-model.md) and [secret-model.md](secret-model.md)).

### Client-Held / BYOK Decrypt Over The Wire

True client-held decrypt over the wire — where a client (or the operator's own
KMS) unwraps the per-secret/field DEK so a remote backend returns only
ciphertext — is post-v0. This applies only to the sealed plane. V0 remote
backends (managed and self-hosted) are server-mediated and advertise
`client_side_decrypt: false` on the sealed plane; local-dev mode decrypts with a
local passphrase-derived key. The envelope and capability seams for this already
exist (see the V0 crypto subset in [key-hierarchy.md](key-hierarchy.md) and
[encryption-model.md](encryption-model.md)).

Promotion changes how `secret reveal` and `totp code` return values, so the
reveal ceremony, audit attribution, and the `server_side_decrypt` flag on
reveal/code events must stay coherent across both decrypt modes. The open plane
is unaffected: memories and facts are ordinary data-at-rest and do not flow
through the envelope.

### Per-Realm Cryptographic Isolation

Cryptographic isolation between realms against a compromised backend deployment
role (least-privilege per-realm KMS grants or per-realm CMKs) is post-v0. V0
isolates tenants by authorization and `realm_id` query scoping, accepting a
tenant-wide blast radius under a single CMK plus a single deployment role
(see [encryption-model.md](encryption-model.md) and [storage.md](storage.md)).
Each realm already has its own KEK; promotion tightens the grant boundary around
those KEKs rather than introducing a new key tier.

### Plaintext Secret Export As Break-Glass

Plaintext export of sealed-plane secrets is post-v0 and should be treated as a
high-risk break-glass feature if it is ever built. It is explicitly distinct from
the v0 plaintext identity export, which covers only the open plane (memories and
facts) and never includes secrets (see
[backup-and-recovery.md](backup-and-recovery.md)). V0 secret backup is
encrypted-only (envelope plus KMS key identity, never plaintext).

Any future plaintext secret export must require deliberate operator
authorization, strong confirmation, an audited reason, clear warnings,
least-privilege controls, redaction rules, and separate documentation from the
normal encrypted backup and restore flows. It must not be reachable through
`witself export`, digest emit, ingest, or the self-digest, all of which remain
sealed-plane-free.

## Deferred Commercial Surfaces

### Witself Utility Token

A Witself utility token is research only and is excluded from both v0 and v1
(see [requirements.md](requirements.md)). It is not a v1 candidate. It must not
be required for account setup, billing, agent access, memory recall, fact reads,
messaging, CLI use, or MCP use.

Traditional payment methods and provider-mediated crypto payment rails can exist
without a Witself-specific token (see [billing-and-limits.md](billing-and-limits.md)).

## Identity-Tied Feature Candidates

These are the RECOMMENDED tier from the agent-identity research: post-v0
candidates that lean directly on Witself's self/identity store rather than on
generic IAM or memory features. They are grouped into three tiers — a cheap
foundation, a flagship whitespace where Witself does what no IdP or memory
product does, and a relationship layer that bridges the IAM world into the
self-store. Each builds on existing Witself concepts and is gated by the
[promotion criteria](#promotion-criteria) below (threat-model update,
permissions/scopes, audit events, CLI/MCP/API contracts, capability flags, and
tests).

### Foundation Tier

Cheap, table-stakes substrate that the rest render from.

#### Typed Identity-Profile Schema

A validated identity-profile layer over facts (stable-id, display-name, owner,
sponsor, issuer/publisher, type/blueprint, model, lifecycle-state, and
classification attrs) that matches the de-facto enterprise/Agent-Card
vocabulary. Identity/self-centric because it names who the agent *is* as
structured, queryable identity rather than loose facts. Fit is high and effort
is small. Builds on [facts-model.md](facts-model.md) and the `primary` flag.

#### Stable Self-ID + Keypair (Self-Sovereign Root)

A durable Witself identifier the agent can prove control of, with optional
DID / SPIFFE / passport renderings; crypto signing lives in the harness/tooling,
never the LLM. Identity/self-centric because it gives the self a self-sovereign,
verifiable root rather than a backend-assigned handle. Effort is medium. Builds
on the agent/token model and is the enabler for signed cards, `/whois`, and
attestations.

### Flagship Whitespace Tier

Where Witself does what no IdP or memory product does.

#### Graduated Disclosure (Policy-Gated Self-Views)

Serve a minimal public self/card versus an authenticated "extended" self,
decided by the requester's group and the default-deny policy engine.
Identity/self-centric because the self decides how much of itself to reveal to
whom. Effort is medium with high differentiation. Builds on
[access-policy.md](access-policy.md) and [security-groups.md](security-groups.md).

#### Values / Constitution Store With Hard Constraints

An ordered list of principles with an explicit priority hierarchy plus a
non-overridable hard-constraints set, recallable and enforceable; guardrails-as-
policy fuse with the access-policy engine. Identity/self-centric because values
and constraints are the durable core of who the agent is, not transient memory.
Effort is medium. Builds on [access-policy.md](access-policy.md) and memory edit
history (see [memory-model.md](memory-model.md)).

#### Persona / Self-Concept Block

A structured persona object (self-summary, traits, context-scoped voice/tone,
and expertise) as first-class identity. Identity/self-centric because it is the
agent's own self-concept rather than data about the world. Fit is high and
effort is small. Builds on the facts/memory model and export/import (see
[facts-model.md](facts-model.md) and [memory-model.md](memory-model.md)).

### Relationship-Layer Tier

Bridges the IAM world into the self-store.

#### Principal Binding & Delegation Graph

Owner principal, sponsor with escalation, delegator(s), an ordered delegation
chain, authority scope, and validity window; Witself holds the binding metadata
and audit linkage — not bearer secrets — so policy can condition on delegation
state. Identity/self-centric because it records *whose* authority the self acts
under as part of the self-store. Effort is medium. Builds on
[access-policy.md](access-policy.md),
[authorization-and-roles.md](authorization-and-roles.md), and the audit ledger.

#### /whois Challenge-Response + Agent Passport

A self-issued passport plus a challenge-response so a peer can verify identity
and permissions before trusting a message; rides inter-agent messaging.
Identity/self-centric because it lets one self prove who it is to another.
Effort is medium. Builds on the stable self-ID/keypair and
[inter-agent-messaging.md](inter-agent-messaging.md).

### Wider Backlog

A further set of candidates was surfaced (capability/skill manifest, signed
Agent-Card projection, per-counterparty trust ledger, vouch/attestation store,
consent grants as enforced data, verifiable forgetting/crypto-shredding,
bitemporal facts, typed entity graph, and C2PA content-provenance) and is
tracked as a backlog to prioritize later. Lightweight provenance is already
seeded in v0 via the `source` field on memories/facts (see
[data-model.md](data-model.md) and [memory-model.md](memory-model.md)).

## Promotion Criteria

A post-v0 feature should move into an active release plan only when it has:

- A written threat-model update. For open-plane features this is framed around
  integrity and authenticity of identity data (memory-poisoning, unauthorized
  curation/forgetting, cross-agent write abuse, sender spoofing); for
  sealed-plane features it is framed around secret confidentiality (leakage,
  KMS/role compromise, reveal abuse, server-side-decrypt TCB expansion, and
  tenant blast radius).
- Clear principal, permission, and policy behavior, including how it interacts
  with the default-deny cross-agent policy engine.
- CLI, API, MCP, and JSON contract changes where applicable.
- Audit events that avoid memory content, fact values, message bodies/payloads,
  embedding vectors, secret values, TOTP seeds, raw tokens, and high-risk payment
  data.
- Capability flags and deterministic `unsupported_operation` behavior.
- Managed and self-hosted support boundaries.
- Migration, backup, and recovery impact notes, including embedding-vector
  round-trip where recall is affected.
- Tests, CI gates, and release notes.
- A rollout plan that can be disabled or limited by backend policy.

## Related Docs

- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [threat-model.md](threat-model.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [storage.md](storage.md)
- [cloud-targets.md](cloud-targets.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [billing-and-limits.md](billing-and-limits.md)
- [implementation-plan.md](implementation-plan.md)
