# Witself Docs

Status: draft. These docs define Witself before implementation. Last reviewed
2026-06-26.

Witself is the agent durable-state platform: one open plane (memories + facts,
plaintext at rest, recallable, in the self-digest, plaintext-exportable) and one
sealed plane (secrets + TOTP, envelope-encrypted, reveal-gated, never embedded,
recalled, in the digest, or plaintext-exported).

## Product And Architecture

- [requirements.md](requirements.md): product requirements, terminology,
  identity posture, billing posture, backend requirements, and settled v0
  decisions.
- [v0-scope.md](v0-scope.md): first release target, required capabilities,
  capability-gated surfaces, non-goals, and exit criteria.
- [memory-model.md](memory-model.md): the memory payload, lifecycle, edit
  history, semantic recall, and the embedding-provider abstraction.
- [facts-model.md](facts-model.md): name→value facts, deterministic lookup,
  primary promotion, and sensitivity/redaction posture.
- [secret-model.md](secret-model.md): the sealed-plane secret data model and
  lifecycle (create/show/reveal/update/rename/copy/archive/restore/delete/grant),
  templates, references, password generation, runtime injection, and the
  carve-outs (secrets are never embedded, recalled, in the digest, or
  plaintext-exported; reveal-gated).
- [totp-2fa.md](totp-2fa.md): the sealed-plane TOTP authenticator
  (enroll/code/show/delete), otpauth/Base32/QR handling, the seed as high-value
  sealed material, and `totp:enroll` vs `totp:code`.
- [secret-size-and-attachments.md](secret-size-and-attachments.md): secret field
  size limits and attachment handling for the sealed plane.
- [context-hydration.md](context-hydration.md): the always-loaded self-digest,
  the agent teaching layer, the two-way CLAUDE.md/AGENTS.md file bridge, and the
  session bootstrap protocol.
- [access-policy.md](access-policy.md): the default-deny cross-agent policy
  engine, permission verbs, guardrails, and `policy test` evaluation.
- [authorization-and-roles.md](authorization-and-roles.md): the role/scope model,
  account and realm roles, scope bundles, and the resolution algorithm spanning
  the open plane (memory/fact/policy/group/message) and the sealed plane
  (secret/totp + grants).
- [security-groups.md](security-groups.md): named agent groups as policy
  subjects and targets, membership, and group-owned shared identity data.
- [inter-agent-messaging.md](inter-agent-messaging.md): the durable mailbox
  model, token-derived sender identity, delivery/ack state, and message threats.
- [operator-auth.md](operator-auth.md): CLI-initiated human/operator auth,
  device-code fallback, self-hosted bootstrap, and unattended token posture.
- [threat-model.md](threat-model.md): assets, principals, trust boundaries,
  the integrity/authenticity attacker model, required controls, and non-goals.
- [backend-architecture.md](backend-architecture.md): public backend,
  self-hosting, storage adapters, process boundaries, and implementation path.
- [observability-and-operations.md](observability-and-operations.md):
  Prometheus metrics, Kubernetes health probes, structured logs, Helm values,
  and operational checks.
- [self-hosting.md](self-hosting.md): intended operator experience for running
  Witself in a customer-owned cloud.
- [self-host-support.md](self-host-support.md): local, preview, and paid
  production self-hosted support boundary.
- [server-command-surface.md](server-command-surface.md): separate
  `witself-server` backend service command design.
- [api-contract.md](api-contract.md): public `/v1` HTTP API shape,
  authentication, capabilities, idempotency, pagination, and route groups.
- [api-routes.md](api-routes.md): resource-oriented `/v1` route style and
  sensitive action route conventions.
- [data-model.md](data-model.md): the full relational data model across both
  planes — realm/account/operator/agent/token tables, open-plane tables
  (memories with the pgvector embedding column, facts, policies, groups,
  messages, audit, usage), and sealed-plane tables (secrets, secret_fields,
  secret_grants, totp_enrollments, realm_keys, secret_deks, attachments).
- [storage.md](storage.md): Postgres-with-pgvector storage, object/blob usage,
  the embedding-provider boundary, KMS plus realm_keys and secret_deks for the
  sealed plane, and Goose migrations.
- [encryption-model.md](encryption-model.md): the sealed-plane confidentiality
  model — envelope encryption, per-realm KEK, hybrid client-side/server-side
  decrypt, and the reveal posture (scoped to the sealed plane only).
- [key-hierarchy.md](key-hierarchy.md): the CMK → per-realm KEK → per-secret/field
  DEK key hierarchy, KMS providers, rotation, and the crypto-shred posture.
- [cloud-targets.md](cloud-targets.md): AWS-first managed cloud and
  self-hosted Terraform target decision.
- [token-lifecycle.md](token-lifecycle.md): durable v0 agent token behavior,
  token file format, rotation, revocation, and agent disable effects.
- [billing-and-limits.md](billing-and-limits.md): account-level plan billing,
  realm-rolled metered dimensions, limits, and overage behavior.
- [audit-retention.md](audit-retention.md): managed, self-hosted, and local
  audit retention defaults and audit content rules.
- [backup-and-recovery.md](backup-and-recovery.md): operational backups
  (including vector data), first-class plaintext identity export, and
  round-trippable import.
- [post-v0-roadmap.md](post-v0-roadmap.md): deliberately deferred features,
  including MCP network transport, web dashboard, utility token, cross-realm
  federation, policy `deny` effects, and automated re-embedding.
- [helm-chart.md](helm-chart.md): first self-hosted Kubernetes deployment
  artifact and chart requirements.
- [terraform-infrastructure.md](terraform-infrastructure.md): AWS, GCP, and
  Azure infrastructure modules and stack layout.
- [governance-and-support.md](governance-and-support.md): public-code,
  licensing, contribution, self-hosting, package, and support boundaries.

## Interfaces

- [workflow-scripts.md](workflow-scripts.md): step-by-step CLI workflow scripts
  for install, setup, billing, memories, facts, policy, groups, messaging, MCP,
  self-hosted, and local mode.
- [cli-command-surface.md](cli-command-surface.md): human and agent CLI command
  design.
- [mcp-tools.md](mcp-tools.md): MCP tool surface and safety posture.
- [json-contracts.md](json-contracts.md): shared machine-readable response
  shapes, `witself://` references, and resource contracts for CLI, MCP, managed
  API, self-hosted API, and local development.

## Delivery

- [release-and-build.md](release-and-build.md): Go baseline, CI, release,
  signing, Homebrew, universal installer, and container image requirements.
- [implementation-plan.md](implementation-plan.md): build sequence for the CLI,
  MCP adapter, backend server, images, Helm, Terraform, release, and hardening.
- [scaffold-readiness.md](scaffold-readiness.md): docs freeze boundary and
  initial repo scaffold target.
- [security-policy.md](security-policy.md): vulnerability reporting and
  supported security surfaces.

## Research

- [competitive-analysis.md](competitive-analysis.md): product patterns from
  agent-memory systems, knowledge stores, identity/profile services, and
  MCP-adjacent tools.

## Decisions

- [decisions/0001-consolidate-witpass-into-witself.md](decisions/0001-consolidate-witpass-into-witself.md):
  consolidating the Witpass secrets product into Witself as the sealed plane —
  the two-plane model, naming/ownership unification, and the five reconciliations.
