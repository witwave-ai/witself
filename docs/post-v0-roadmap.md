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
- First-class structured/plaintext identity export and round-trippable import.
- Audit events.
- JSON, API, and MCP contracts.
- Public images, Helm charts under `charts/*`, Terraform, CI, linting, and
  release automation.

The features below should be documented now and revisited after v0 hardening.
They are also previewed in [requirements.md](requirements.md).

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
remains a PII/redaction flag, not an encryption boundary.

## Deferred Commercial Surfaces

### Witself Utility Token

A Witself utility token is research only and is excluded from both v0 and v1
(see [requirements.md](requirements.md)). It is not a v1 candidate. It must not
be required for account setup, billing, agent access, memory recall, fact reads,
messaging, CLI use, or MCP use.

Traditional payment methods and provider-mediated crypto payment rails can exist
without a Witself-specific token (see [billing-and-limits.md](billing-and-limits.md)).

## Promotion Criteria

A post-v0 feature should move into an active release plan only when it has:

- A written threat-model update, framed around integrity and authenticity of
  identity data (memory-poisoning, unauthorized curation/forgetting, cross-agent
  write abuse, sender spoofing) rather than secret confidentiality.
- Clear principal, permission, and policy behavior, including how it interacts
  with the default-deny cross-agent policy engine.
- CLI, API, MCP, and JSON contract changes where applicable.
- Audit events that avoid memory content, fact values, message bodies/payloads,
  embedding vectors, raw tokens, and high-risk payment data.
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
